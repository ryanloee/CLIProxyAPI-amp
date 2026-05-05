package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	codebuddyauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// CodebuddyAPIBaseURL is the default upstream API endpoint for Tencent Codebuddy.
	CodebuddyAPIBaseURL = "https://www.codebuddy.ai"

	// CodebuddyCNAPIBaseURL is the base URL for Codebuddy CN (domestic China).
	CodebuddyCNAPIBaseURL = "https://copilot.tencent.com"
)

// CodebuddyExecutor is a stateless executor for Tencent Codebuddy API using OpenAI-compatible chat completions.
type CodebuddyExecutor struct {
	ClaudeExecutor
	cfg *config.Config
}

// NewCodebuddyExecutor creates a new Codebuddy executor.
func NewCodebuddyExecutor(cfg *config.Config) *CodebuddyExecutor {
	return &CodebuddyExecutor{cfg: cfg}
}

// Identifier returns the executor identifier.
func (e *CodebuddyExecutor) Identifier() string { return "codebuddy" }

// CodebuddyIntlExecutor wraps CodebuddyExecutor with a different identifier for the international version.
type CodebuddyIntlExecutor struct {
	*CodebuddyExecutor
}

// NewCodebuddyIntlExecutor creates a new Codebuddy Intl executor.
func NewCodebuddyIntlExecutor(cfg *config.Config) *CodebuddyIntlExecutor {
	return &CodebuddyIntlExecutor{CodebuddyExecutor: NewCodebuddyExecutor(cfg)}
}

// Identifier returns the executor identifier for the international version.
func (e *CodebuddyIntlExecutor) Identifier() string { return "codebuddy-intl" }

// PrepareRequest injects Codebuddy credentials into the outgoing HTTP request.
func (e *CodebuddyExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	uid, token := codebuddyCreds(auth)
	applyCodebuddyBearerToken(req, uid, token)
	if domain := codebuddyDomain(auth); domain != "" {
		req.Header.Set("X-Domain", domain)
	}
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codebuddy credentials into the request and executes it.
func (e *CodebuddyExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codebuddy executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// codebuddyBaseURL resolves the API base URL from auth domain, attributes, or falls back to the default.
func codebuddyBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return v
		}
	}
	domain := codebuddyDomain(auth)
	if domain == "copilot.tencent.com" || domain == "www.codebuddy.cn" {
		return CodebuddyCNAPIBaseURL
	}
	return CodebuddyAPIBaseURL
}

// Execute performs a non-streaming chat completion request to Codebuddy.
// CodeBuddy's native clients call the chat endpoint in streaming mode. Some
// CodeBuddy environments reject non-streaming requests, so non-streaming client
// calls are fulfilled by streaming upstream and aggregating the SSE chunks.
func (e *CodebuddyExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	from := opts.SourceFormat
	if from.String() == "claude" {
		auth.Attributes["base_url"] = codebuddyBaseURL(auth)
		uid, token := codebuddyCreds(auth)
		auth.Attributes["api_key"] = codebuddyBearerToken(uid, token)
		return e.ClaudeExecutor.Execute(ctx, auth, req, opts)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	uid, token := codebuddyCreds(auth)
	domain := codebuddyDomain(auth)

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	// Strip codebuddy- prefix for upstream API
	upstreamModel := stripCodebuddyPrefix(baseModel)
	body, err = sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return resp, fmt.Errorf("codebuddy executor: failed to set model in payload: %w", err)
	}

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "openai", e.Identifier())
	if err != nil {
		return resp, err
	}
	body = applyCodebuddyDefaultReasoning(body, baseModel, e.Identifier())
	body = ensureIntlSystemMessage(body, isCodebuddyIntl(auth))

	body, err = sjson.SetBytes(body, "stream", true)
	if err != nil {
		return resp, fmt.Errorf("codebuddy executor: failed to set stream in payload: %w", err)
	}
	body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return resp, fmt.Errorf("codebuddy executor: failed to set stream_options in payload: %w", err)
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel, requestPath)

	baseURL := codebuddyBaseURL(auth)
	url := strings.TrimRight(baseURL, "/") + "/v2/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	applyCodebuddyHeaders(httpReq, uid, token, true, domain)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codebuddy executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	data = aggregateCodebuddyStream(data, baseModel)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming chat completion request to Codebuddy.
func (e *CodebuddyExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	from := opts.SourceFormat
	if from.String() == "claude" {
		auth.Attributes["base_url"] = codebuddyBaseURL(auth)
		uid, token := codebuddyCreds(auth)
		auth.Attributes["api_key"] = codebuddyBearerToken(uid, token)
		return e.ClaudeExecutor.ExecuteStream(ctx, auth, req, opts)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	uid, token := codebuddyCreds(auth)
	domain := codebuddyDomain(auth)

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	// Strip codebuddy- prefix for upstream API
	upstreamModel := stripCodebuddyPrefix(baseModel)
	body, err = sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, fmt.Errorf("codebuddy executor: failed to set model in payload: %w", err)
	}

	intl := isCodebuddyIntl(auth)
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "openai", e.Identifier())
	if err != nil {
		return nil, err
	}
	body = applyCodebuddyDefaultReasoning(body, baseModel, e.Identifier())
	body = ensureIntlSystemMessage(body, intl)

	body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return nil, fmt.Errorf("codebuddy executor: failed to set stream_options in payload: %w", err)
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel, requestPath)

	baseURL := codebuddyBaseURL(auth)
	url := strings.TrimRight(baseURL, "/") + "/v2/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyCodebuddyHeaders(httpReq, uid, token, true, domain)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codebuddy executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codebuddy executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576) // 1MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
		}
		doneChunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param)
		for i := range doneChunks {
			out <- cliproxyexecutor.StreamChunk{Payload: doneChunks[i]}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates token count for Codebuddy requests.
func (e *CodebuddyExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	auth.Attributes["base_url"] = codebuddyBaseURL(auth)
	return e.ClaudeExecutor.CountTokens(ctx, auth, req, opts)
}

// Refresh refreshes the Codebuddy token using the refresh token.
func (e *CodebuddyExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codebuddy executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("codebuddy executor: auth is nil")
	}
	// For API key-based auth, there's nothing to refresh
	refreshToken := ""
	accessToken := ""
	domain := ""
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			refreshToken = v
		}
		if v, ok := auth.Metadata["access_token"].(string); ok {
			accessToken = v
		}
		if v, ok := auth.Metadata["domain"].(string); ok {
			domain = v
		}
	}
	if strings.TrimSpace(refreshToken) == "" {
		return auth, nil
	}

	authSvc := codebuddyauth.NewCodebuddyAuth(e.cfg)
	tokenData, err := authSvc.RefreshToken(ctx, accessToken, refreshToken, domain)
	if err != nil {
		return nil, fmt.Errorf("codebuddy executor: refresh failed: %w", err)
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
		exp := time.Unix(tokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
		auth.Metadata["expired"] = exp
		auth.Metadata["expires_at"] = tokenData.ExpiresAt
	}
	if tokenData.Domain != "" {
		auth.Metadata["domain"] = tokenData.Domain
	}
	auth.Metadata["type"] = "codebuddy"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	return auth, nil
}

// applyCodebuddyHeaders sets required headers for Codebuddy API requests.
func applyCodebuddyHeaders(r *http.Request, uid, token string, stream bool, domain string) {
	r.Header.Set("Content-Type", "application/json")
	applyCodebuddyBearerToken(r, uid, token)
	if domain != "" {
		r.Header.Set("X-Domain", domain)
	}
	if uid != "" {
		r.Header.Set("X-User-Id", uid)
	}
	r.Header.Set("User-Agent", "CodebuddyCode/1.0")
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		return
	}
	r.Header.Set("Accept", "application/json")
}

// applyCodebuddyBearerToken sets the Authorization header with Bearer token and X-User-Id.
func applyCodebuddyBearerToken(r *http.Request, uid, token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if uid != "" {
		r.Header.Set("X-User-Id", uid)
	}
}

// codebuddyBearerToken returns the bearer token for the Authorization header.
func codebuddyBearerToken(uid, token string) string {
	_ = uid
	return token
}

// codebuddyCreds extracts uid and access token from auth.
func codebuddyCreds(a *cliproxyauth.Auth) (uid, token string) {
	if a == nil {
		return "", ""
	}
	// Check metadata first (OAuth flow stores tokens here)
	if a.Metadata != nil {
		if v, ok := a.Metadata["uid"].(string); ok {
			uid = v
		}
		if v, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			token = v
		}
	}
	// Fallback to attributes (API key style)
	if a.Attributes != nil {
		if v := a.Attributes["access_token"]; v != "" {
			token = v
		}
		if v := a.Attributes["api_key"]; v != "" {
			token = v
		}
	}
	return uid, token
}

// codebuddyDomain extracts the domain from auth metadata.
func codebuddyDomain(a *cliproxyauth.Auth) string {
	if a != nil && a.Metadata != nil {
		if v, ok := a.Metadata["domain"].(string); ok {
			return v
		}
	}
	return ""
}

// isCodebuddyIntl returns true when the auth belongs to the Codebuddy
// International endpoint (www.codebuddy.ai), which requires streaming-only
// requests and a mandatory system message.
func isCodebuddyIntl(auth *cliproxyauth.Auth) bool {
	return codebuddyDomain(auth) == "www.codebuddy.ai"
}

// ensureIntlSystemMessage injects a default system message into the request
// body when the Codebuddy International API is in use and no system message
// is present. The international server rejects requests without a system role.
func ensureIntlSystemMessage(body []byte, intl bool) []byte {
	if !intl {
		return body
	}
	// Check if any message already has role "system"
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body
	}
	for _, m := range messages.Array() {
		if m.Get("role").String() == "system" {
			return body
		}
	}
	// Prepend a default system message by rebuilding the array
	raw := gjson.GetBytes(body, "messages").Raw
	newMsgs := `[{"role":"system","content":"You are a helpful assistant."}` + "," + raw[1:]
	result, err := sjson.SetRawBytes(body, "messages", []byte(newMsgs))
	if err != nil {
		log.WithField("error", err).Debug("codebuddy-intl: failed to inject system message")
		return body
	}
	log.Debug("codebuddy-intl: injected default system message")
	return result
}

// stripCodebuddyPrefix removes the "codebuddy-" prefix from model names for the upstream API.
func stripCodebuddyPrefix(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(model), "codebuddy-") {
		return model[10:]
	}
	return model
}

// applyCodebuddyDefaultReasoning injects default reasoning_effort for CodeBuddy models
// that support thinking but have no explicit config (no suffix, no body parameter).
// This matches the native CodeBuddy extension behavior: reasoning_effort="medium" by default.
func applyCodebuddyDefaultReasoning(body []byte, baseModel string, providerKey string) []byte {
	body = normalizeCodebuddyReasoningEffort(body)
	if gjson.GetBytes(body, "reasoning_effort").Exists() {
		return body
	}
	modelInfo := registry.LookupModelInfo(baseModel, providerKey)
	if modelInfo == nil || modelInfo.Thinking == nil {
		return body
	}
	result, err := sjson.SetBytes(body, "reasoning_effort", defaultCodebuddyReasoningEffort(baseModel))
	if err != nil {
		return body
	}
	log.WithFields(log.Fields{
		"model":    baseModel,
		"provider": providerKey,
	}).Debug("codebuddy: applied default reasoning_effort=medium |")
	return result
}

func normalizeCodebuddyReasoningEffort(body []byte) []byte {
	if strings.EqualFold(gjson.GetBytes(body, "reasoning_effort").String(), "xhigh") {
		if result, err := sjson.SetBytes(body, "reasoning_effort", "high"); err == nil {
			return result
		}
	}
	return body
}

func defaultCodebuddyReasoningEffort(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "deepseek-v3.2", "deepseek-v3-2-volc", "glm-4.6v", "hy3-preview", "kimi-k2.5", "kimi-k2-thinking", "minimax-m2.5":
		return "high"
	case "gemini-3.1-flash-lite", "glm-5.0-turbo", "glm-5.1", "glm-5v-turbo", "kimi-k2.6", "minimax-m2.7":
		return "medium"
	default:
		return "medium"
	}
}

type codebuddyStreamChoice struct {
	Role            string
	Content         strings.Builder
	Reasoning       strings.Builder
	FinishReason    string
	NativeFinish    string
	ToolCalls       map[int]*codebuddyStreamToolCall
	ToolCallIndexes []int
}

type codebuddyStreamToolCall struct {
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

func aggregateCodebuddyStream(data []byte, fallbackModel string) []byte {
	choices := map[int]*codebuddyStreamChoice{}
	choiceIndexes := []int{}
	id := ""
	model := fallbackModel
	created := time.Now().Unix()
	usageRaw := ""

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 1_048_576)
	for scanner.Scan() {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		for strings.HasPrefix(raw, "data:") {
			raw = strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
		}
		if raw == "" || raw == "[DONE]" || !gjson.Valid(raw) {
			continue
		}
		chunk := gjson.Parse(raw)
		if v := chunk.Get("id").String(); v != "" && id == "" {
			id = v
		}
		if v := chunk.Get("model").String(); v != "" {
			model = v
		}
		if v := chunk.Get("created").Int(); v > 0 {
			created = v
		}
		if u := chunk.Get("usage"); u.Exists() && u.Raw != "" && u.Raw != "null" {
			usageRaw = u.Raw
		}
		for _, c := range chunk.Get("choices").Array() {
			index := int(c.Get("index").Int())
			agg := choices[index]
			if agg == nil {
				agg = &codebuddyStreamChoice{ToolCalls: map[int]*codebuddyStreamToolCall{}}
				choices[index] = agg
				choiceIndexes = append(choiceIndexes, index)
			}
			delta := c.Get("delta")
			if role := delta.Get("role").String(); role != "" {
				agg.Role = role
			}
			agg.Content.WriteString(delta.Get("content").String())
			agg.Reasoning.WriteString(delta.Get("reasoning_content").String())
			if finish := c.Get("finish_reason").String(); finish != "" {
				agg.FinishReason = finish
			}
			if finish := c.Get("native_finish_reason").String(); finish != "" {
				agg.NativeFinish = finish
			}
			for pos, tc := range delta.Get("tool_calls").Array() {
				toolIndex := int(tc.Get("index").Int())
				if !tc.Get("index").Exists() {
					toolIndex = pos
				}
				tool := agg.ToolCalls[toolIndex]
				if tool == nil {
					tool = &codebuddyStreamToolCall{}
					agg.ToolCalls[toolIndex] = tool
					agg.ToolCallIndexes = append(agg.ToolCallIndexes, toolIndex)
				}
				if v := tc.Get("id").String(); v != "" {
					tool.ID = v
				}
				if v := tc.Get("type").String(); v != "" {
					tool.Type = v
				}
				if v := tc.Get("function.name").String(); v != "" {
					tool.Name = v
				}
				tool.Arguments.WriteString(tc.Get("function.arguments").String())
			}
		}
	}

	if id == "" {
		id = "chatcmpl-codebuddy"
	}
	sort.Ints(choiceIndexes)
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": buildCodebuddyNonStreamChoices(choiceIndexes, choices),
	}
	if usageRaw != "" {
		var usage any
		if err := json.Unmarshal([]byte(usageRaw), &usage); err == nil {
			out["usage"] = usage
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return []byte(`{"object":"chat.completion","choices":[]}`)
	}
	return encoded
}

func buildCodebuddyNonStreamChoices(indexes []int, choices map[int]*codebuddyStreamChoice) []map[string]any {
	out := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		choice := choices[index]
		if choice == nil {
			continue
		}
		role := choice.Role
		if role == "" {
			role = "assistant"
		}
		message := map[string]any{
			"role":    role,
			"content": choice.Content.String(),
		}
		if reasoning := choice.Reasoning.String(); reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		if len(choice.ToolCalls) > 0 {
			sort.Ints(choice.ToolCallIndexes)
			tools := make([]map[string]any, 0, len(choice.ToolCallIndexes))
			for _, toolIndex := range choice.ToolCallIndexes {
				tool := choice.ToolCalls[toolIndex]
				if tool == nil {
					continue
				}
				toolType := tool.Type
				if toolType == "" {
					toolType = "function"
				}
				tools = append(tools, map[string]any{
					"id":    tool.ID,
					"type":  toolType,
					"index": toolIndex,
					"function": map[string]any{
						"name":      tool.Name,
						"arguments": tool.Arguments.String(),
					},
				})
			}
			message["tool_calls"] = tools
		}
		out = append(out, map[string]any{
			"index":                index,
			"message":              message,
			"finish_reason":        emptyToNil(choice.FinishReason),
			"native_finish_reason": emptyToNil(choice.NativeFinish),
		})
	}
	return out
}

func emptyToNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}
