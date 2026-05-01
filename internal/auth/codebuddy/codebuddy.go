// Package codebuddy provides authentication and token management for Tencent Codebuddy API.
// It handles the OAuth device flow for secure authentication.
package codebuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	CodebuddyCNEndpoint = "https://www.codebuddy.cn"
	CodebuddyIntlEndpoint = "https://www.codebuddy.ai"
	codebuddyAPIPrefix = "/v2/plugin"
	codebuddyPlatform = "ide"
	defaultPollInterval = 2 * time.Second
	maxPollDuration = 10 * time.Minute
	oauthTimeoutSeconds = 600
)

// CodebuddyAuth handles Codebuddy authentication flow.
type CodebuddyAuth struct {
	httpClient  *http.Client
	cfg         *config.Config
	baseURL     string
}

// NewCodebuddyAuth creates a new CodebuddyAuth for Chinese region (codebuddy.cn).
func NewCodebuddyAuth(cfg *config.Config) *CodebuddyAuth {
	return newCodebuddyAuthWithEndpoint(cfg, CodebuddyCNEndpoint)
}

// NewCodebuddyIntlAuth creates a new CodebuddyAuth for international region (codebuddy.ai).
func NewCodebuddyIntlAuth(cfg *config.Config) *CodebuddyAuth {
	return newCodebuddyAuthWithEndpoint(cfg, CodebuddyIntlEndpoint)
}

func newCodebuddyAuthWithEndpoint(cfg *config.Config, endpoint string) *CodebuddyAuth {
	client := &http.Client{Timeout: 30 * time.Second}
	var effectiveProxyURL string
	if cfg != nil {
		effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if effectiveProxyURL != "" {
		sdkCfg := config.SDKConfig{ProxyURL: effectiveProxyURL}
		client = util.SetProxy(&sdkCfg, client)
	}
	return &CodebuddyAuth{
		httpClient: client,
		cfg:        cfg,
		baseURL:    endpoint,
	}
}

// StartLogin initiates the OAuth login flow by requesting an auth state from Codebuddy.
// Returns a DeviceCodeResponse with the verification URL for the user.
func (c *CodebuddyAuth) StartLogin(ctx context.Context) (*DeviceCodeResponse, error) {
	url := fmt.Sprintf("%s%s/auth/state?platform=%s", c.baseURL, codebuddyAPIPrefix, codebuddyPlatform)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return nil, fmt.Errorf("codebuddy: failed to create auth state request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CodebuddyCode/1.0")
	req.Header.Set("X-Msh-Platform", "codebuddy_cli")
	req.Header.Set("X-Msh-Device-Name", getHostname())
	req.Header.Set("X-Msh-Device-Model", getDeviceModel())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: auth state request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: failed to read auth state response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codebuddy: auth state request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var apiResp struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(bodyBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("codebuddy: failed to parse auth state response: %w", err)
	}

	var data struct {
		State  string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err = json.Unmarshal(apiResp.Data, &data); err != nil {
		return nil, fmt.Errorf("codebuddy: failed to parse auth state data: %w", err)
	}

	state := strings.TrimSpace(data.State)
	if state == "" {
		return nil, fmt.Errorf("codebuddy: auth state response missing state field")
	}

	verificationURI := strings.TrimSpace(data.AuthURL)
	if verificationURI == "" {
		verificationURI = fmt.Sprintf("%s/login?state=%s", c.baseURL, state)
	}

	loginID := generateLoginID()

	return &DeviceCodeResponse{
		LoginID:                loginID,
		State:                  state,
		VerificationURI:        verificationURI,
		VerificationURIComplete: verificationURI,
		ExpiresIn:              oauthTimeoutSeconds,
		Interval:               int(defaultPollInterval.Seconds()),
	}, nil
}

// WaitForAuthorization polls the token endpoint until the user authorizes or the flow expires.
func (c *CodebuddyAuth) WaitForAuthorization(ctx context.Context, deviceCode *DeviceCodeResponse) (*CodebuddyAuthBundle, error) {
	if deviceCode == nil {
		return nil, fmt.Errorf("codebuddy: device code is nil")
	}

	interval := defaultPollInterval
	deadline := time.Now().Add(maxPollDuration)
	if deviceCode.ExpiresIn > 0 {
		codeDeadline := time.Now().Add(time.Duration(deviceCode.ExpiresIn) * time.Second)
		if codeDeadline.Before(deadline) {
			deadline = codeDeadline
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("codebuddy: context cancelled: %w", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("codebuddy: login timed out")
			}

			token, pollErr := c.pollToken(ctx, deviceCode.State)
			if pollErr != nil {
				// Not ready yet, continue polling
				if strings.Contains(pollErr.Error(), "not authorized") ||
					strings.Contains(pollErr.Error(), "pending") {
					continue
				}
				return nil, pollErr
			}
			if token != nil {
				// Fetch account info with the new token
				accountInfo, err := c.fetchAccountInfo(ctx, token.AccessToken, deviceCode.State, token.Domain)
				if err != nil {
					log.Warnf("codebuddy: failed to fetch account info: %v", err)
				}

				return &CodebuddyAuthBundle{
					TokenData:   token,
					AccountInfo: accountInfo,
				}, nil
			}
		}
	}
}

// pollToken checks if the user has authorized the login request.
func (c *CodebuddyAuth) pollToken(ctx context.Context, state string) (*CodebuddyTokenData, error) {
	url := fmt.Sprintf("%s%s/auth/token?state=%s", c.baseURL, codebuddyAPIPrefix, state)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: failed to create token poll request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CodebuddyCode/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: token poll request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: failed to read token response: %w", err)
	}

	var apiResp struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(bodyBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("codebuddy: failed to parse token response: %w", err)
	}

	// Code != 0 and != 200 means not authorized yet or error
	if apiResp.Code != 0 && apiResp.Code != 200 {
		return nil, fmt.Errorf("not authorized yet: code=%d", apiResp.Code)
	}

	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		TokenType    string `json:"tokenType"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
	}
	if err = json.Unmarshal(apiResp.Data, &data); err != nil {
		// Try snake_case
		var dataSnake struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			TokenType    string `json:"token_type"`
			ExpiresAt    int64  `json:"expires_at"`
			Domain       string `json:"domain"`
		}
		if err2 := json.Unmarshal(apiResp.Data, &dataSnake); err2 != nil {
			return nil, fmt.Errorf("not authorized yet: parse error")
		}
		data.AccessToken = dataSnake.AccessToken
		data.RefreshToken = dataSnake.RefreshToken
		data.TokenType = dataSnake.TokenType
		data.ExpiresAt = dataSnake.ExpiresAt
		data.Domain = dataSnake.Domain
	}

	if data.AccessToken == "" {
		return nil, fmt.Errorf("not authorized yet: empty token")
	}

	return &CodebuddyTokenData{
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		TokenType:    data.TokenType,
		ExpiresAt:    data.ExpiresAt,
		Domain:       data.Domain,
	}, nil
}

// fetchAccountInfo retrieves user account information after successful authentication.
func (c *CodebuddyAuth) fetchAccountInfo(ctx context.Context, accessToken, state, domain string) (*CodebuddyAccountInfo, error) {
	url := fmt.Sprintf("%s%s/login/account?state=%s", c.baseURL, codebuddyAPIPrefix, state)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: failed to create account info request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: account info request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: failed to read account info response: %w", err)
	}

	var apiResp struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(bodyBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("codebuddy: failed to parse account info: %w", err)
	}

	info := &CodebuddyAccountInfo{}
	_ = json.Unmarshal(apiResp.Data, info)
	return info, nil
}

// RefreshToken refreshes an access token using the refresh token.
func (c *CodebuddyAuth) RefreshToken(ctx context.Context, accessToken, refreshToken, domain string) (*CodebuddyTokenData, error) {
	url := fmt.Sprintf("%s%s/auth/token/refresh", c.baseURL, codebuddyAPIPrefix)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return nil, fmt.Errorf("codebuddy: failed to create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Refresh-Token", refreshToken)
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: refresh request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: failed to read refresh response: %w", err)
	}

	var apiResp struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(bodyBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("codebuddy: failed to parse refresh response: %w", err)
	}

	if apiResp.Code != 0 && apiResp.Code != 200 {
		return nil, fmt.Errorf("codebuddy: refresh failed (code=%d): %s", apiResp.Code, apiResp.Message)
	}

	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		TokenType    string `json:"tokenType"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
	}
	if err = json.Unmarshal(apiResp.Data, &data); err != nil {
		var dataSnake struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			TokenType    string `json:"token_type"`
			ExpiresAt    int64  `json:"expires_at"`
			Domain       string `json:"domain"`
		}
		if err2 := json.Unmarshal(apiResp.Data, &dataSnake); err2 != nil {
			return nil, fmt.Errorf("codebuddy: failed to parse refresh data: %w", err)
		}
		data.AccessToken = dataSnake.AccessToken
		data.RefreshToken = dataSnake.RefreshToken
		data.TokenType = dataSnake.TokenType
		data.ExpiresAt = dataSnake.ExpiresAt
		data.Domain = dataSnake.Domain
	}

	return &CodebuddyTokenData{
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		TokenType:    data.TokenType,
		ExpiresAt:    data.ExpiresAt,
		Domain:       data.Domain,
	}, nil
}

func generateLoginID() string {
	id := uuid.New()
	return fmt.Sprintf("cb_%s", strings.ReplaceAll(id.String(), "-", ""))
}

func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

func getDeviceModel() string {
	return fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)
}
