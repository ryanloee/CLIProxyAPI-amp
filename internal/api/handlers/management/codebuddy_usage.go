package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	codebuddyauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	codebuddyBillingTimeout = 15 * time.Second
)

type codebuddyUsageResult struct {
	Provider      string                       `json:"provider"`
	Label         string                       `json:"label,omitempty"`
	AuthID        string                       `json:"auth_id"`
	Domain        string                       `json:"domain,omitempty"`
	UID           string                       `json:"uid,omitempty"`
	PlanType      string                       `json:"plan_type,omitempty"`
	DosageNotify  *codebuddyDosageNotifyData   `json:"dosage_notify,omitempty"`
	PaymentType   string                       `json:"payment_type,omitempty"`
	UserResources *codebuddyUserResourceData   `json:"user_resources,omitempty"`
	Error         string                       `json:"error,omitempty"`
}

type codebuddyDosageNotifyData struct {
	Code string `json:"code"`
	Zh   string `json:"zh,omitempty"`
	En   string `json:"en,omitempty"`
}

type codebuddyUserResourceData struct {
	TotalCount int64                        `json:"total_count,omitempty"`
	Packages   []codebuddyResourcePackage   `json:"packages,omitempty"`
}

type codebuddyResourcePackage struct {
	PackageName string `json:"package_name,omitempty"`
	Total       int64  `json:"total"`
	Remain      int64  `json:"remain"`
	Used        int64  `json:"used"`
	Status      int    `json:"status"`
	StartTime   string `json:"start_time,omitempty"`
	EndTime     string `json:"end_time,omitempty"`
	CycleRemain int64  `json:"cycle_remain,omitempty"`
	CycleTotal  int64  `json:"cycle_total,omitempty"`
}

func codebuddyBillingBaseURL(auth *coreauth.Auth) string {
	domain := extractCodebuddyDomain(auth)
	if domain == "copilot.tencent.com" || domain == "www.codebuddy.cn" {
		return "https://www.codebuddy.cn"
	}
	return "https://www.codebuddy.ai"
}

func extractCodebuddyCreds(a *coreauth.Auth) (uid, token, domain string) {
	if a == nil {
		return "", "", ""
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["uid"].(string); ok {
			uid = v
		}
		if v, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			token = v
		}
		if v, ok := a.Metadata["domain"].(string); ok {
			domain = v
		}
	}
	if a.Attributes != nil {
		if v := a.Attributes["access_token"]; v != "" && token == "" {
			token = v
		}
		if v := a.Attributes["api_key"]; v != "" && token == "" {
			token = v
		}
	}
	return uid, token, domain
}

func extractCodebuddyDomain(a *coreauth.Auth) string {
	if a != nil && a.Metadata != nil {
		if v, ok := a.Metadata["domain"].(string); ok {
			return v
		}
	}
	return ""
}

func newCodebuddyBillingHTTPClient(cfg *config.Config) *http.Client {
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
		Timeout:   codebuddyBillingTimeout,
		Transport: transport,
	}
}

func makeCodebuddyBillingRequest(ctx context.Context, client *http.Client, method, url string, body []byte, token, uid, domain string) ([]byte, error) {
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
	req.Header.Set("Authorization", "Bearer "+token)
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	}

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

func queryDosageNotify(ctx context.Context, client *http.Client, baseURL, token, uid, domain string) (*codebuddyDosageNotifyData, error) {
	url := strings.TrimRight(baseURL, "/") + "/v2/billing/meter/get-dosage-notify"
	data, err := makeCodebuddyBillingRequest(ctx, client, http.MethodPost, url, []byte("{}"), token, uid, domain)
	if err != nil {
		// 404 means the endpoint is not available for this region/account
		if strings.Contains(err.Error(), "http 404") {
			return nil, nil
		}
		return nil, err
	}
	code := gjson.GetBytes(data, "code").Int()
	if code != 0 && code != 200 {
		return nil, fmt.Errorf("api code %d: %s", code, gjson.GetBytes(data, "message").String())
	}
	dosageData := gjson.GetBytes(data, "data")
	if !dosageData.Exists() {
		return nil, nil
	}
	return &codebuddyDosageNotifyData{
		Code: dosageData.Get("dosageNotifyCode").String(),
		Zh:   dosageData.Get("dosageNotifyZh").String(),
		En:   dosageData.Get("dosageNotifyEn").String(),
	}, nil
}

func queryPaymentType(ctx context.Context, client *http.Client, baseURL, token, uid, domain string) (string, error) {
	url := strings.TrimRight(baseURL, "/") + "/v2/billing/meter/get-payment-type"
	data, err := makeCodebuddyBillingRequest(ctx, client, http.MethodPost, url, []byte("{}"), token, uid, domain)
	if err != nil {
		return "", err
	}
	code := gjson.GetBytes(data, "code").Int()
	if code != 0 && code != 200 {
		return "", fmt.Errorf("api code %d: %s", code, gjson.GetBytes(data, "message").String())
	}
	paymentData := gjson.GetBytes(data, "data")
	if !paymentData.Exists() {
		return "", nil
	}
	pt := paymentData.Get("paymentType").String()
	if pt == "" {
		pt = paymentData.String()
	}
	return pt, nil
}

func queryUserResource(ctx context.Context, client *http.Client, baseURL, token, uid, domain string) (*codebuddyUserResourceData, error) {
	url := strings.TrimRight(baseURL, "/") + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	reqBody, _ := json.Marshal(map[string]any{
		"PageNumber":  1,
		"PageSize":    100,
		"ProductCode": "p_tcaca",
		"Status":      []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 24 * 100 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	data, err := makeCodebuddyBillingRequest(ctx, client, http.MethodPost, url, reqBody, token, uid, domain)
	if err != nil {
		return nil, err
	}
	code := gjson.GetBytes(data, "code").Int()
	if code != 0 && code != 200 {
		return nil, fmt.Errorf("api code %d: %s", code, gjson.GetBytes(data, "message").String())
	}
	// Actual CN API response: data.Response.Data.TotalCount / Accounts[]
	// Fallback flat format: data.TotalCount / Packages[]
	accounts := gjson.GetBytes(data, "data.Response.Data.Accounts")
	totalCount := gjson.GetBytes(data, "data.Response.Data.TotalCount")
	if !accounts.IsArray() {
		accounts = gjson.GetBytes(data, "data.Accounts")
	}
	if totalCount.Int() == 0 {
		totalCount = gjson.GetBytes(data, "data.TotalCount")
	}
	if !accounts.IsArray() {
		return nil, nil
	}
	result := &codebuddyUserResourceData{
		TotalCount: totalCount.Int(),
	}
	for _, pkg := range accounts.Array() {
		// Cycle-level usage (Precise string fields first, then integer fields, then package-level fallback)
		cycleTotal := preciseOrInt(pkg, "CycleCapacitySizePrecise", "CycleCapacitySize", "CapacitySizePrecise", "CapacitySize")
		cycleRemain := preciseOrInt(pkg, "CycleCapacityRemainPrecise", "CycleCapacityRemain", "CapacityRemainPrecise", "CapacityRemain")
		used := cycleTotal - cycleRemain
		result.Packages = append(result.Packages, codebuddyResourcePackage{
			PackageName: pkg.Get("PackageName").String(),
			Total:       cycleTotal,
			Remain:      cycleRemain,
			Used:        used,
			Status:      int(pkg.Get("Status").Int()),
			StartTime:   pkg.Get("CycleStartTime").String(),
			EndTime:     pkg.Get("CycleEndTime").String(),
			CycleRemain: cycleRemain,
			CycleTotal:  cycleTotal,
		})
	}
	return result, nil
}

func (h *Handler) tryRefreshCodebuddyToken(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
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

	accessToken, _ := auth.Metadata["access_token"].(string)
	domain, _ := auth.Metadata["domain"].(string)

	var authSvc *codebuddyauth.CodebuddyAuth
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "codebuddy" {
		authSvc = codebuddyauth.NewCodebuddyAuth(cfg)
	} else {
		authSvc = codebuddyauth.NewCodebuddyIntlAuth(cfg)
	}

	tokenData, err := authSvc.RefreshToken(ctx, accessToken, refreshToken, domain)
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

func (h *Handler) GetCodebuddyUsage(c *gin.Context) {
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

	client := newCodebuddyBillingHTTPClient(cfg)

	var (
		resultsMu sync.Mutex
		results   []codebuddyUsageResult
	)

	auths := manager.List()
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 3)

	for _, auth := range auths {
		if auth == nil {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider != "codebuddy" && provider != "codebuddy-intl" && provider != "trae" {
			continue
		}

		uid, token, domain := extractCodebuddyCreds(auth)
		if strings.TrimSpace(token) == "" {
			continue
		}

		wg.Add(1)
		go func(auth *coreauth.Auth, provider, uid, token, domain string) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			result := codebuddyUsageResult{
				Provider: provider,
				Label:    auth.Label,
				AuthID:   auth.ID,
				Domain:   domain,
				UID:      uid,
			}

			refreshed, refreshErr := h.tryRefreshCodebuddyToken(ctx, auth)
			if refreshErr != nil {
				result.Error = fmt.Sprintf("token refresh: %v", refreshErr)
			}
			if refreshed != nil && refreshed.Metadata != nil {
				if t, ok := refreshed.Metadata["access_token"].(string); ok && t != "" {
					token = t
				}
			}

			baseURL := codebuddyBillingBaseURL(auth)
			if provider == "trae" {
				baseURL = "https://www.codebuddy.ai"
			}

			var errs []string

			dosage, err := queryDosageNotify(ctx, client, baseURL, token, uid, domain)
			if err != nil {
				errs = append(errs, fmt.Sprintf("dosage: %v", err))
			}
			result.DosageNotify = dosage

			payment, err := queryPaymentType(ctx, client, baseURL, token, uid, domain)
			if err != nil {
				errs = append(errs, fmt.Sprintf("payment: %v", err))
			}
			result.PaymentType = payment

			resources, err := queryUserResource(ctx, client, baseURL, token, uid, domain)
			if err != nil {
				errs = append(errs, fmt.Sprintf("resources: %v", err))
			}
			result.UserResources = resources

			if len(errs) > 0 {
				if result.Error != "" {
					result.Error += "; "
				}
				result.Error += strings.Join(errs, "; ")
			}

			resultsMu.Lock()
			results = append(results, result)
			resultsMu.Unlock()
		}(auth, provider, uid, token, domain)
	}

	wg.Wait()

	if results == nil {
		results = []codebuddyUsageResult{}
	}
	c.JSON(http.StatusOK, gin.H{"usage": results})
}

func preciseOrInt(pkg gjson.Result, keys ...string) int64 {
	for _, key := range keys {
		v := pkg.Get(key)
		if s := strings.TrimSpace(v.String()); s != "" {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n
			}
		}
		if n := v.Int(); n != 0 {
			return n
		}
	}
	return 0
}

func summarizeBody(data []byte) string {
	if len(data) > 200 {
		return string(data[:200]) + "..."
	}
	return string(data)
}
