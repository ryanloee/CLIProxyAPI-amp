package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const (
	authClientID      = "ono9krqynydwx5"
	exchangeSecret    = "-"
	defaultPluginVer  = "local"
	minAppVersion     = "3.5.54"
	defaultAppType    = "stable"
	defaultDeviceID   = "0"
	oauthTimeout      = 600 * time.Second
	callbackPath      = "/authorize"
	authorizationPath = "/authorization"
	exchangeTokenPath = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	getUserInfoPath   = "/cloudide/api/v3/trae/GetUserInfo"
)

var loginGuidanceURLs = []string{
	"https://api.marscode.com/cloudide/api/v3/trae/GetLoginGuidance",
	"https://api.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
	"https://www.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
}

var fallbackAPIOrigins = []string{
	"https://api.marscode.com",
	"https://api.trae.ai",
	"https://www.trae.ai",
	"https://www.marscode.com",
}

const (
	callbackSuccessHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Trae Login</title></head><body><h2>Trae login successful</h2><p>You can return to CLIProxyAPI.</p></body></html>`
	callbackPendingHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Trae Login</title></head><body><h2>Waiting for authorization...</h2><p id="hint">Please complete login in your browser.</p><script>(function(){if(window.location.hash&&window.location.hash.length>1){var hash=window.location.hash.slice(1);var target=window.location.origin+window.location.pathname+'?'+hash;window.location.replace(target);return;}document.getElementById('hint').textContent='No authorization parameters detected.';})();</script></body></html>`
	callbackFailureHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Trae Login</title></head><body><h2>Trae login failed</h2><p>%s</p></body></html>`
)

// TraeAuth handles Trae international OAuth authentication.
type TraeAuth struct {
	httpClient *http.Client
	cfg        *config.Config
}

// NewTraeAuth creates a new TraeAuth.
func NewTraeAuth(cfg *config.Config) *TraeAuth {
	client := &http.Client{Timeout: 30 * time.Second}
	if cfg != nil {
		if proxy := strings.TrimSpace(cfg.ProxyURL); proxy != "" {
			client = util.SetProxy(&config.SDKConfig{ProxyURL: proxy}, client)
		}
	}
	return &TraeAuth{httpClient: client, cfg: cfg}
}

// StartLogin initiates the Trae OAuth login flow.
// Returns a DeviceCodeResponse with the verification URL and callback channel.
func (t *TraeAuth) StartLogin(ctx context.Context) (*DeviceCodeResponse, error) {
	loginTraceID := uuid.New().String()

	loginHost, err := t.requestLoginGuidance(ctx, loginTraceID)
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("trae: %w", err)
	}
	callbackPort := ln.Addr().(*net.TCPAddr).Port
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d%s", callbackPort, callbackPath)

	lc := t.collectLoginContext()
	verificationURI, err := buildVerificationURI(loginHost, loginTraceID, callbackURL, lc)
	if err != nil {
		return nil, fmt.Errorf("trae: %w", err)
	}

	loginID := uuid.New().String()

	resultCh := make(chan callbackResult, 1)
	go t.runCallbackServer(loginID, ln, loginHost, resultCh)

	return &DeviceCodeResponse{
		LoginID:                 loginID,
		State:                   loginTraceID,
		VerificationURI:         verificationURI,
		VerificationURIComplete: verificationURI,
		ExpiresIn:               int(oauthTimeout.Seconds()),
		Interval:                1,
		CallbackPort:            callbackPort,
		CallbackResultCh:        resultCh,
		LoginHost:               loginHost,
	}, nil
}

// WaitForAuthorization waits for the user to authorize via the callback server.
func (t *TraeAuth) WaitForAuthorization(ctx context.Context, deviceCode *DeviceCodeResponse) (*TraeAuthBundle, error) {
	if deviceCode == nil || deviceCode.CallbackResultCh == nil {
		return nil, fmt.Errorf("trae: device code is nil")
	}

	deadline := time.Now().Add(time.Duration(deviceCode.ExpiresIn) * time.Second)

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("trae: context cancelled: %w", ctx.Err())
	case result, ok := <-deviceCode.CallbackResultCh:
		if !ok {
			return nil, fmt.Errorf("trae: callback channel closed")
		}
		if result.Err != nil {
			return nil, fmt.Errorf("trae: %w", result.Err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("trae: login timed out")
		}
		return t.completeAuth(ctx, deviceCode, &result)
	}
}

// RefreshToken refreshes an access token using the refresh token.
func (t *TraeAuth) RefreshToken(ctx context.Context, refreshToken, loginHost string) (*TraeTokenData, error) {
	body := map[string]string{
		"ClientID":     authClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": exchangeSecret,
		"UserID":       "",
	}
	jsonBody, _ := json.Marshal(body)

	resp, err := t.postToAPI(ctx, loginHost, exchangeTokenPath, jsonBody, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("trae: read exchange response: %w", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("trae: parse exchange response: %w", err)
	}

	accessToken := extractExchangeAccessToken(raw)
	if accessToken == "" {
		return nil, fmt.Errorf("trae: exchange response missing access token")
	}

	newRefresh := extractExchangeRefreshToken(raw)
	tokenType := extractExchangeTokenType(raw)
	expiresAt := extractExchangeExpiresAt(raw)

	if newRefresh == "" {
		newRefresh = refreshToken
	}

	return &TraeTokenData{
		AccessToken:  accessToken,
		RefreshToken: newRefresh,
		TokenType:    tokenType,
		ExpiresAt:    expiresAt,
		LoginHost:    loginHost,
	}, nil
}

// --- internal ---

type callbackResult struct {
	RefreshToken  string
	LoginHost     string
	LoginRegion   string
	LoginTraceID  string
	CloudIDEToken string
	RawQuery      map[string]string
	Err           error
}

func (t *TraeAuth) completeAuth(ctx context.Context, deviceCode *DeviceCodeResponse, result *callbackResult) (*TraeAuthBundle, error) {
	exchangeBody := map[string]string{
		"ClientID":     authClientID,
		"RefreshToken": result.RefreshToken,
		"ClientSecret": exchangeSecret,
		"UserID":       "",
	}
	jsonBody, _ := json.Marshal(exchangeBody)

	resp, err := t.postToAPI(ctx, result.LoginHost, exchangeTokenPath, jsonBody, result.CloudIDEToken)
	if err != nil {
		return nil, fmt.Errorf("trae: exchange token: %w", err)
	}
	defer resp.Body.Close()

	exchangeData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("trae: read exchange response: %w", err)
	}

	var exchangeRaw map[string]any
	if err := json.Unmarshal(exchangeData, &exchangeRaw); err != nil {
		return nil, fmt.Errorf("trae: parse exchange response: %w", err)
	}

	accessToken := extractExchangeAccessToken(exchangeRaw)
	if accessToken == "" {
		errMsg := pickString(exchangeRaw,
			[]string{"message"}, []string{"msg"}, []string{"error"},
			[]string{"errorMsg"}, []string{"error_msg"},
			[]string{"ResponseMetadata", "Error", "Message"},
			[]string{"Result", "Message"}, []string{"result", "message"})
		if errMsg != "" {
			return nil, fmt.Errorf("trae: exchange token failed: %s", errMsg)
		}
		return nil, fmt.Errorf("trae: exchange response missing access token")
	}

	refreshToken := extractExchangeRefreshToken(exchangeRaw)
	if refreshToken == "" {
		refreshToken = result.RefreshToken
	}

	tokenType := extractExchangeTokenType(exchangeRaw)
	expiresAt := extractExchangeExpiresAt(exchangeRaw)

	tokenData := &TraeTokenData{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    tokenType,
		ExpiresAt:    expiresAt,
		LoginHost:    result.LoginHost,
	}

	accountInfo, err := t.fetchUserInfo(ctx, result.LoginHost, accessToken)
	if err != nil {
		accountInfo = &TraeAccountInfo{}
	}

	return &TraeAuthBundle{
		TokenData:   tokenData,
		AccountInfo: accountInfo,
		LoginHost:   result.LoginHost,
		LoginRegion: inferLoginRegion(result.LoginRegion, result.LoginHost),
	}, nil
}

func (t *TraeAuth) fetchUserInfo(ctx context.Context, loginHost, accessToken string) (*TraeAccountInfo, error) {
	resp, err := t.postToAPI(ctx, loginHost, getUserInfoPath, []byte("{}"), accessToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("trae: read user info: %w", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("trae: parse user info: %w", err)
	}

	info := &TraeAccountInfo{}
	info.Email = pickString(raw,
		[]string{"Result", "NonPlainTextEmail"}, []string{"Result", "Email"},
		[]string{"Result", "email"}, []string{"NonPlainTextEmail"},
		[]string{"result", "email"}, []string{"data", "email"},
		[]string{"data", "user", "email"}, []string{"email"})
	info.UserID = pickString(raw,
		[]string{"Result", "UserID"}, []string{"Result", "userId"},
		[]string{"Result", "UID"},
		[]string{"result", "userId"}, []string{"result", "uid"},
		[]string{"data", "userId"}, []string{"data", "uid"},
		[]string{"userId"}, []string{"uid"})
	info.Nickname = pickString(raw,
		[]string{"Result", "ScreenName"}, []string{"Result", "Nickname"},
		[]string{"Result", "nickname"}, []string{"Result", "Name"},
		[]string{"result", "nickname"}, []string{"result", "name"},
		[]string{"data", "nickname"}, []string{"data", "name"},
		[]string{"nickname"}, []string{"name"})
	return info, nil
}

func (t *TraeAuth) requestLoginGuidance(ctx context.Context, loginTraceID string) (string, error) {
	body := map[string]string{
		"loginTraceID":   loginTraceID,
		"login_trace_id": loginTraceID,
	}
	jsonBody, _ := json.Marshal(body)

	for _, endpoint := range loginGuidanceURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBody))
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Trae/1.0.0 CLIProxyAPI")

		resp, err := t.httpClient.Do(req)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			continue
		}

		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}

		if host := pickString(raw,
			[]string{"Result", "LoginHost"}, []string{"Result", "loginHost"},
			[]string{"Result", "LoginURL"}, []string{"Result", "loginUrl"},
			[]string{"result", "LoginHost"}, []string{"result", "loginHost"},
			[]string{"result", "loginUrl"},
			[]string{"data", "Result", "LoginHost"}, []string{"data", "result", "loginHost"},
			[]string{"data", "loginHost"}, []string{"data", "loginUrl"},
			[]string{"LoginHost"}, []string{"loginHost"},
			[]string{"loginUrl"}); host != "" {
			return host, nil
		}
	}
	return "", fmt.Errorf("failed to get Trae login guidance from all endpoints")
}

type loginContext struct {
	PluginVersion string
	MachineID     string
	DeviceID      string
	DeviceBrand   string
	DeviceType    string
	OSVersion     string
	Env           string
	AppVersion    string
	AppType       string
}

func (t *TraeAuth) collectLoginContext() *loginContext {
	return &loginContext{
		PluginVersion: defaultPluginVer,
		MachineID:     uuid.New().String(),
		DeviceID:      defaultDeviceID,
		DeviceBrand:   detectDeviceBrand(),
		DeviceType:    detectDeviceType(),
		OSVersion:     detectOSVersion(),
		Env:           "",
		AppVersion:    minAppVersion,
		AppType:       defaultAppType,
	}
}

func (t *TraeAuth) runCallbackServer(_ string, ln net.Listener, fallbackHost string, resultCh chan<- callbackResult) {
	defer close(resultCh)
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		params := r.URL.Query()

		if errCode := firstNonEmpty(params, "error", "error_code", "err", "errorCode"); errCode != "" {
			errDesc := firstNonEmpty(params, "error_description", "error_desc", "message")
			msg := errCode
			if errDesc != "" {
				msg = fmt.Sprintf("%s (%s)", errCode, errDesc)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, callbackFailureHTML, msg)
			resultCh <- callbackResult{Err: fmt.Errorf("authorization failed: %s", msg)}
			return
		}

		isRedirect := firstNonEmpty(params, "isRedirect", "is_redirect", "redirect")
		if isRedirect != "true" && isRedirect != "1" {
			if r.URL.RawQuery == "" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, callbackPendingHTML)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, callbackFailureHTML, "callback missing isRedirect=true")
			resultCh <- callbackResult{Err: fmt.Errorf("callback missing isRedirect=true")}
			return
		}

		refreshToken := firstNonEmpty(params, "refreshToken", "refresh_token", "RefreshToken", "refresh-token")
		if refreshToken == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, callbackFailureHTML, "callback missing refreshToken")
			resultCh <- callbackResult{Err: fmt.Errorf("callback missing refreshToken")}
			return
		}

		loginHost := firstNonEmpty(params, "loginHost", "login_host", "LoginHost", "host", "consoleHost")
		if loginHost == "" {
			loginHost = fallbackHost
		}

		loginRegion := firstNonEmpty(params, "loginRegion", "login_region", "region", "Region")
		loginTraceID := firstNonEmpty(params, "loginTraceID", "loginTraceId", "login_trace_id", "trace_id")
		cloudideToken := extractCloudIDEToken(params)

		rawQuery := make(map[string]string)
		for k, v := range params {
			if len(v) > 0 {
				rawQuery[k] = v[0]
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, callbackSuccessHTML)

		resultCh <- callbackResult{
			RefreshToken:  refreshToken,
			LoginHost:     loginHost,
			LoginRegion:   loginRegion,
			LoginTraceID:  loginTraceID,
			CloudIDEToken: cloudideToken,
			RawQuery:      rawQuery,
		}
	})

	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	_ = server.Serve(ln)
}

func (t *TraeAuth) postToAPI(ctx context.Context, loginHost, path string, body []byte, cloudideToken string) (*http.Response, error) {
	urls := buildAPIURLs(loginHost, path)
	var lastErr error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		if cloudideToken != "" {
			req.Header.Set("x-cloudide-token", cloudideToken)
		}

		resp, err := t.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("%s => HTTP %d: %s", u, resp.StatusCode, string(respBody))
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("all API endpoints failed: %w", lastErr)
}

// --- utility functions ---

func buildVerificationURI(loginHost, loginTraceID, callbackURL string, lc *loginContext) (string, error) {
	host := strings.TrimSpace(loginHost)
	if host == "" {
		return "", fmt.Errorf("login host is empty")
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "https://" + strings.TrimPrefix(host, "/")
	}

	parsed, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("invalid login host: %w", err)
	}
	parsed.Path = authorizationPath

	q := parsed.Query()
	q.Set("login_version", "1")
	q.Set("auth_from", "trae")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", lc.PluginVersion)
	q.Set("auth_type", "local")
	q.Set("client_id", authClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", loginTraceID)
	q.Set("auth_callback_url", callbackURL)
	q.Set("machine_id", lc.MachineID)
	q.Set("device_id", lc.DeviceID)
	q.Set("x_device_id", lc.DeviceID)
	q.Set("x_machine_id", lc.MachineID)
	q.Set("x_device_brand", lc.DeviceBrand)
	q.Set("x_device_type", lc.DeviceType)
	q.Set("x_os_version", lc.OSVersion)
	q.Set("x_env", lc.Env)
	q.Set("x_app_version", lc.AppVersion)
	q.Set("x_app_type", lc.AppType)
	parsed.RawQuery = q.Encode()

	return parsed.String(), nil
}

func buildAPIURLs(loginHost, path string) []string {
	seen := make(map[string]bool)
	var urls []string

	if parsed, err := url.Parse(strings.TrimSpace(loginHost)); err == nil && parsed.Host != "" {
		origin := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
		u := origin + path
		if !seen[u] {
			urls = append(urls, u)
			seen[u] = true
		}
		if strings.HasPrefix(parsed.Host, "www.") {
			apiOrigin := fmt.Sprintf("%s://api.%s", parsed.Scheme, strings.TrimPrefix(parsed.Host, "www."))
			u = apiOrigin + path
			if !seen[u] {
				urls = append(urls, u)
				seen[u] = true
			}
		}
	}

	for _, origin := range fallbackAPIOrigins {
		u := origin + path
		if !seen[u] {
			urls = append(urls, u)
			seen[u] = true
		}
	}
	return urls
}

func pickString(raw map[string]any, paths ...[]string) string {
	for _, path := range paths {
		current := any(raw)
		for _, key := range path {
			m, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = m[key]
		}
		if s, ok := current.(string); ok {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func pickInt64(raw map[string]any, paths ...[]string) int64 {
	for _, path := range paths {
		current := any(raw)
		for _, key := range path {
			m, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = m[key]
		}
		switch v := current.(type) {
		case float64:
			return int64(v)
		case json.Number:
			if i, err := v.Int64(); err == nil {
				return i
			}
		case string:
			var n int64
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				return n
			}
		}
	}
	return 0
}

func firstNonEmpty(params url.Values, keys ...string) string {
	for _, k := range keys {
		if v := params.Get(k); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func inferLoginRegion(region, loginHost string) string {
	if region != "" {
		lower := strings.ToLower(region)
		if lower == "cn" || lower == "sg" || lower == "us" {
			return lower
		}
	}
	host := strings.ToLower(loginHost)
	if strings.Contains(host, ".cn") {
		return "cn"
	}
	if strings.Contains(host, ".us") {
		return "us"
	}
	return "sg"
}

func detectDeviceType() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		return "unknown"
	}
}

func detectDeviceBrand() string {
	switch runtime.GOOS {
	case "darwin":
		return "Mac"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return "unknown"
	}
}


// extractExchangeAccessToken extracts access token from exchange response (mirrors Rust).
func extractExchangeAccessToken(raw map[string]any) string {
	return pickString(raw,
		[]string{"Result", "AccessToken"}, []string{"Result", "accessToken"},
		[]string{"Result", "Token"}, []string{"Result", "token"},
		[]string{"result", "accessToken"}, []string{"result", "access_token"},
		[]string{"result", "Token"}, []string{"result", "token"},
		[]string{"data", "accessToken"}, []string{"data", "access_token"},
		[]string{"data", "Token"}, []string{"data", "token"},
		[]string{"Token"}, []string{"accessToken"},
		[]string{"access_token"}, []string{"token"})
}

// extractExchangeRefreshToken extracts refresh token from exchange response.
func extractExchangeRefreshToken(raw map[string]any) string {
	return pickString(raw,
		[]string{"Result", "RefreshToken"}, []string{"Result", "refreshToken"},
		[]string{"result", "refreshToken"}, []string{"result", "refresh_token"},
		[]string{"data", "refreshToken"}, []string{"data", "refresh_token"},
		[]string{"refreshToken"}, []string{"refresh_token"})
}

// extractExchangeTokenType extracts token type from exchange response.
func extractExchangeTokenType(raw map[string]any) string {
	return pickString(raw,
		[]string{"Result", "TokenType"}, []string{"Result", "tokenType"},
		[]string{"result", "tokenType"}, []string{"result", "token_type"},
		[]string{"data", "tokenType"}, []string{"data", "token_type"},
		[]string{"tokenType"}, []string{"token_type"})
}

// extractExchangeExpiresAt extracts expires timestamp from exchange response.
func extractExchangeExpiresAt(raw map[string]any) int64 {
	return pickInt64(raw,
		[]string{"Result", "ExpiresAt"}, []string{"Result", "expiresAt"},
		[]string{"Result", "expiredAt"},
		[]string{"result", "expiresAt"}, []string{"result", "expires_at"},
		[]string{"data", "expiresAt"}, []string{"data", "expires_at"},
		[]string{"expiresAt"}, []string{"expires_at"})
}

// extractCloudIDEToken extracts the cloudide token from callback params,
// including parsing it from a userJwt JSON string (mirrors Rust extract_cloudide_token).
func extractCloudIDEToken(params url.Values) string {
	if token := firstNonEmpty(params,
		"x-cloudide-token", "xCloudideToken",
		"accessToken", "access_token", "token"); token != "" {
		return token
	}
	userJwt := firstNonEmpty(params, "userJwt", "user_jwt")
	if userJwt == "" {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(userJwt), &parsed); err != nil {
		return ""
	}
	return pickString(parsed,
		[]string{"Token"}, []string{"token"},
		[]string{"AccessToken"}, []string{"accessToken"},
		[]string{"access_token"})
}

func detectOSVersion() string {
	return fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
}
