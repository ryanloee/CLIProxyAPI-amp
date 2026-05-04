package management

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/trae"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const traeUsageTimeout = 15 * time.Second

type traeUsageResult struct {
	Provider    string        `json:"provider"`
	Label       string        `json:"label,omitempty"`
	AuthID      string        `json:"auth_id"`
	LoginRegion string        `json:"login_region,omitempty"`
	PlanType    string        `json:"plan_type,omitempty"`
	PlanResetAt int64         `json:"plan_reset_at,omitempty"`
	Packs       []traePackInfo `json:"packs,omitempty"`
	Error       string        `json:"error,omitempty"`
}

type traePackInfo struct {
	ProductType     int    `json:"product_type"`
	ProductName     string `json:"product_name"`
	BasicUsageLimit int64  `json:"basic_usage_limit"`
	BasicUsed       int64  `json:"basic_usage_amount"`
	BonusUsageLimit int64  `json:"bonus_usage_limit"`
	BonusUsed       int64  `json:"bonus_usage_amount"`
	EndTime         int64  `json:"end_time,omitempty"`
}

func traeBaseURL(region string) string {
	switch strings.ToUpper(strings.TrimSpace(region)) {
	case "SG":
		return "https://growsg-normal.trae.ai"
	case "US":
		return "https://growva-normal.trae.ai"
	case "USTTP":
		return "https://grow-normal.traeapi.us"
	default:
		return "https://grow-normal.trae.ai"
	}
}

func traePlanName(productType int) string {
	switch productType {
	case 6:
		return "Ultra"
	case 4:
		return "Pro+"
	case 1:
		return "Pro"
	case 9:
		return "Trial"
	case 8:
		return "Lite"
	case 0:
		return "Free"
	default:
		return fmt.Sprintf("Unknown(%d)", productType)
	}
}

func newTraeUsageHTTPClient(cfg *config.Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg != nil {
		proxyURL := strings.TrimSpace(cfg.ProxyURL)
		if proxyURL != "" {
			if pt := buildProxyTransport(proxyURL); pt != nil {
				transport = pt
			}
		}
	}
	return &http.Client{
		Timeout:   traeUsageTimeout,
		Transport: transport,
	}
}

func makeTraeUsageRequest(ctx context.Context, client *http.Client, method, url string, body []byte, token string) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+token)
	req.Header.Set("User-Agent", "Trae/1.0.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, fmt.Errorf("http %d: %s", resp.StatusCode, summarizeBody(data))
	}
	return data, nil
}

func queryTraePayStatus(ctx context.Context, client *http.Client, baseURL, token string) (string, int64, error) {
	url := strings.TrimRight(baseURL, "/") + "/trae/api/v1/pay/ide_user_pay_status"
	data, err := makeTraeUsageRequest(ctx, client, http.MethodPost, url, []byte("{}"), token)
	if err != nil {
		return "", 0, err
	}
	code := gjson.GetBytes(data, "code").Int()
	if code != 0 {
		return "", 0, fmt.Errorf("pay_status api code %d: %s", code, gjson.GetBytes(data, "message").String())
	}
	planType := gjson.GetBytes(data, "user_pay_identity_str").String()
	var resetAt int64
	detail := gjson.GetBytes(data, "detail")
	if detail.Exists() {
		resetAt = detail.Get("subscription_renew_time").Int()
	}
	return planType, resetAt, nil
}

func queryTraeEntUsage(ctx context.Context, client *http.Client, baseURL, token string) ([]traePackInfo, error) {
	url := strings.TrimRight(baseURL, "/") + "/trae/api/v1/pay/ide_user_ent_usage"
	data, err := makeTraeUsageRequest(ctx, client, http.MethodPost, url, []byte(`{"require_usage": true}`), token)
	if err != nil {
		return nil, err
	}
	code := gjson.GetBytes(data, "code").Int()
	if code != 0 {
		return nil, fmt.Errorf("ent_usage api code %d: %s", code, gjson.GetBytes(data, "message").String())
	}
	packList := gjson.GetBytes(data, "user_entitlement_pack_list")
	if !packList.IsArray() {
		return nil, nil
	}

	// Priority: Ultra(6) > Pro+(4) > Pro(1) > Trial(9) > Lite(8) > Free(0), skip 3
	type packCandidate struct {
		pt   int
		info traePackInfo
	}
	var candidates []packCandidate
	for _, pack := range packList.Array() {
		baseInfo := pack.Get("entitlement_base_info")
		pt := int(baseInfo.Get("product_type").Int())
		if pt == 3 {
			continue
		}
		quota := baseInfo.Get("quota")
		usage := pack.Get("usage")
		endTime := baseInfo.Get("end_time").Int()
		candidates = append(candidates, packCandidate{pt: pt, info: traePackInfo{
			ProductType:     pt,
			ProductName:     traePlanName(pt),
			BasicUsageLimit: quota.Get("basic_usage_limit").Int(),
			BasicUsed:       usage.Get("basic_usage_amount").Int(),
			BonusUsageLimit: quota.Get("bonus_usage_limit").Int(),
			BonusUsed:       usage.Get("bonus_usage_amount").Int(),
			EndTime:         endTime,
		}})
	}

	// Return all packs sorted by priority
	priority := map[int]int{6: 0, 4: 1, 1: 2, 9: 3, 8: 4, 0: 5}
	sorted := make([]traePackInfo, 0, len(candidates))
	for _, order := range []int{6, 4, 1, 9, 8, 0} {
		for _, c := range candidates {
			if c.pt == order {
				sorted = append(sorted, c.info)
			}
		}
	}
	// Append any remaining unknown types
	for _, c := range candidates {
		if _, ok := priority[c.pt]; !ok {
			sorted = append(sorted, c.info)
		}
	}

	return sorted, nil
}

func extractTraeCreds(a *coreauth.Auth) (token, region string) {
	if a == nil {
		return "", ""
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			token = v
		}
		if v, ok := a.Metadata["login_region"].(string); ok {
			region = v
		}
	}
	if a.Attributes != nil {
		if v := a.Attributes["access_token"]; v != "" && token == "" {
			token = v
		}
	}
	return token, region
}

func (h *Handler) GetTraeUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	h.mu.Lock()
	manager := h.authManager
	cfg := h.cfg
	h.mu.Unlock()

	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	client := newTraeUsageHTTPClient(cfg)

	var (
		resultsMu sync.Mutex
		results   []traeUsageResult
	)

	auths := manager.List()
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 3)

	for _, auth := range auths {
		if auth == nil {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider != "trae" {
			continue
		}

		token, region := extractTraeCreds(auth)
		if strings.TrimSpace(token) == "" {
			continue
		}

		wg.Add(1)
		go func(auth *coreauth.Auth, token, region string) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			result := traeUsageResult{
				Provider:    "trae",
				Label:       auth.Label,
				AuthID:      auth.ID,
				LoginRegion: region,
			}

			baseURL := traeBaseURL(region)

			// Try refresh token if expired
			refreshed, refreshErr := h.tryRefreshTraeToken(ctx, auth)
			if refreshErr != nil {
				result.Error = fmt.Sprintf("token refresh: %v", refreshErr)
			}
			if refreshed != nil && refreshed.Metadata != nil {
				if t, ok := refreshed.Metadata["access_token"].(string); ok && t != "" {
					token = t
				}
			}

			var errs []string

			planType, resetAt, err := queryTraePayStatus(ctx, client, baseURL, token)
			if err != nil {
				errs = append(errs, fmt.Sprintf("pay_status: %v", err))
			}
			result.PlanType = planType
			result.PlanResetAt = resetAt

			packs, err := queryTraeEntUsage(ctx, client, baseURL, token)
			if err != nil {
				errs = append(errs, fmt.Sprintf("ent_usage: %v", err))
			}
			result.Packs = packs

			if len(errs) > 0 {
				if result.Error != "" {
					result.Error += "; "
				}
				result.Error += strings.Join(errs, "; ")
			}

			resultsMu.Lock()
			results = append(results, result)
			resultsMu.Unlock()
		}(auth, token, region)
	}

	wg.Wait()

	if results == nil {
		results = []traeUsageResult{}
	}
	c.JSON(http.StatusOK, gin.H{"usage": results})
}

func (h *Handler) tryRefreshTraeToken(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if auth == nil || auth.Metadata == nil {
		return auth, nil
	}
	refreshToken, _ := auth.Metadata["refresh_token"].(string)
	if strings.TrimSpace(refreshToken) == "" {
		return auth, nil
	}

	var needsRefresh bool
	if expStr, ok := auth.Metadata["expired"].(string); ok && expStr != "" {
		if t, err := time.Parse(time.RFC3339, expStr); err == nil && time.Now().After(t) {
			needsRefresh = true
		}
	}
	if !needsRefresh {
		if expAt, ok := auth.Metadata["expires_at"].(float64); ok && expAt > 0 {
			if time.Now().Unix() > int64(expAt) {
				needsRefresh = true
			}
		}
	}
	if !needsRefresh {
		return auth, nil
	}

	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()

	loginHost, _ := auth.Metadata["login_host"].(string)

	authSvc := trae.NewTraeAuth(cfg)
	tokenData, err := authSvc.RefreshToken(ctx, refreshToken, loginHost)
	if err != nil {
		return auth, fmt.Errorf("refresh failed: %w", err)
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if tokenData.AccessToken != "" {
		auth.Metadata["access_token"] = tokenData.AccessToken
	}
	if tokenData.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenData.RefreshToken
	}
	if tokenData.ExpiresAt > 0 {
		auth.Metadata["expires_at"] = tokenData.ExpiresAt
		auth.Metadata["expired"] = time.Unix(tokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	return auth, nil
}
