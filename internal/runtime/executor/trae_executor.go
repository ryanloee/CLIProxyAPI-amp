package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	traeauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/trae"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TraeExecutor implements ProviderExecutor for the Trae international AI API.
type TraeExecutor struct {
	cfg *config.Config
}

// NewTraeExecutor creates a new Trae executor.
func NewTraeExecutor(cfg *config.Config) *TraeExecutor {
	return &TraeExecutor{cfg: cfg}
}

// Identifier returns the provider key.
func (e *TraeExecutor) Identifier() string { return "trae" }

// PrepareRequest injects Trae credentials into the outgoing HTTP request.
func (e *TraeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, userID := traeCreds(auth)
	setTraeHeaders(req, token, userID)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Trae credentials and executes the request.
func (e *TraeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("trae executor: request is nil")
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

// Execute performs a non-streaming chat completion request to Trae.
func (e *TraeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	from := opts.SourceFormat
	baseModel := req.Model

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	traeModel, ok := resolveTraeModel(baseModel)
	if !ok {
		return resp, fmt.Errorf("trae executor: unsupported model: %s", baseModel)
	}

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)

	body, err = sjson.SetBytes(body, "model", traeModel.TraeName)
	if err != nil {
		return resp, fmt.Errorf("trae executor: failed to set model: %w", err)
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel, requestPath)

	traeReq := buildTraeRequest(body, traeModel.TraeName, false)
	payload, err := json.Marshal(traeReq)
	if err != nil {
		return resp, fmt.Errorf("trae executor: failed to marshal request: %w", err)
	}

	url := traeAgentsRunsPath
	if !traeModel.IsNew {
		url = traeChatPath
	}
	fullURL := traeBaseURL + url

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		return resp, err
	}
	e.PrepareRequest(httpReq, auth)

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
		URL: fullURL, Method: http.MethodPost, Headers: httpReq.Header.Clone(),
		Body: payload, Provider: e.Identifier(), AuthID: authID,
		AuthLabel: authLabel, AuthType: authType, AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("trae executor: close response body error: %v", errClose)
		}
	}()

	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		return resp, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	// Trae returns SSE even for non-streaming requests. Parse it.
	fullResponse, fullReasoning, finishReason, err := parseTraeSSEResponse(httpResp.Body, ctx, e.cfg)
	if err != nil {
		return resp, err
	}

	openaiResp := buildOpenAIChatCompletion(baseModel, fullResponse, fullReasoning, finishReason, traeModel.Reasoning)
	resp = cliproxyexecutor.Response{Payload: openaiResp, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming chat completion request to Trae.
func (e *TraeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	from := opts.SourceFormat
	baseModel := req.Model

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	traeModel, ok := resolveTraeModel(baseModel)
	if !ok {
		return nil, fmt.Errorf("trae executor: unsupported model: %s", baseModel)
	}

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	body, err = sjson.SetBytes(body, "model", traeModel.TraeName)
	if err != nil {
		return nil, fmt.Errorf("trae executor: failed to set model: %w", err)
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel, requestPath)

	traeReq := buildTraeRequest(body, traeModel.TraeName, true)
	payload, err := json.Marshal(traeReq)
	if err != nil {
		return nil, fmt.Errorf("trae executor: failed to marshal request: %w", err)
	}

	url := traeAgentsRunsPath
	if !traeModel.IsNew {
		url = traeChatPath
	}
	fullURL := traeBaseURL + url

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	e.PrepareRequest(httpReq, auth)

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
		URL: fullURL, Method: http.MethodPost, Headers: httpReq.Header.Clone(),
		Body: payload, Provider: e.Identifier(), AuthID: authID,
		AuthLabel: authLabel, AuthType: authType, AuthValue: authValue,
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
			log.Errorf("trae executor: close response body error: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("trae executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576)
		chunkIndex := 0
		currentEvent := ""

		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)

			if bytes.HasPrefix(line, []byte("event:")) {
				currentEvent = strings.TrimSpace(string(line[6:]))
				continue
			}
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}

			data := bytes.TrimSpace(line[5:])

			// Handle SSE error events (code 1001, 5003, etc.)
			if currentEvent == "error" {
				errMsg := extractTraeErrorMessage(data)
				errCode := extractTraeErrorCode(data)
				log.Debugf("trae executor: SSE error: code=%d msg=%s", errCode, errMsg)
				out <- cliproxyexecutor.StreamChunk{
					Err: statusErr{code: http.StatusBadGateway, msg: fmt.Sprintf("trae error %d: %s", errCode, errMsg)},
				}
				return
			}

			if currentEvent == "done" {
				break
			}

			// Only process "output" events for content.
			if currentEvent != "output" {
				continue
			}

			var outData traeOutputData
			if err := json.Unmarshal(data, &outData); err != nil {
				continue
			}

			if outData.FinishReason == "stop" || outData.FinishReason == "length" {
				chunk := buildOpenAIStreamChunk(baseModel, "", "", outData.FinishReason, chunkIndex, traeModel.Reasoning)
				chunkIndex++
				out <- cliproxyexecutor.StreamChunk{Payload: chunk}
				break
			}

			if outData.Response != "" || outData.ReasoningContent != "" {
				chunk := buildOpenAIStreamChunk(baseModel, outData.Response, outData.ReasoningContent, "", chunkIndex, traeModel.Reasoning)
				chunkIndex++
				out <- cliproxyexecutor.StreamChunk{Payload: chunk}
			}
		}

		doneChunk := buildOpenAIStreamDone()
		out <- cliproxyexecutor.StreamChunk{Payload: doneChunk}

		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// Refresh refreshes the Trae access token.
func (e *TraeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("trae executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("trae executor: auth is nil")
	}

	var refreshToken, loginHost string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			refreshToken = v
		}
		if v, ok := auth.Metadata["login_host"].(string); ok && strings.TrimSpace(v) != "" {
			loginHost = v
		}
	}
	if strings.TrimSpace(refreshToken) == "" {
		return auth, nil
	}

	authSvc := traeauth.NewTraeAuth(e.cfg)
	tokenData, err := authSvc.RefreshToken(ctx, refreshToken, loginHost)
	if err != nil {
		return nil, err
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokenData.AccessToken
	if tokenData.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenData.RefreshToken
	}
	if tokenData.ExpiresAt > 0 {
		exp := time.Unix(tokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
		auth.Metadata["expired"] = exp
	}
	auth.Metadata["type"] = "trae"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)

	if storage, ok := auth.Storage.(*traeauth.TraeTokenStorage); ok && storage != nil {
		storage.AccessToken = tokenData.AccessToken
		if tokenData.RefreshToken != "" {
			storage.RefreshToken = tokenData.RefreshToken
		}
		if tokenData.ExpiresAt > 0 {
			storage.ExpiresAt = tokenData.ExpiresAt
			storage.Expired = time.Unix(tokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
		}
	}

	return auth, nil
}

// CountTokens is not supported by Trae.
func (e *TraeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, fmt.Errorf("trae executor: token counting is not supported")
}

// --- Internal helpers ---

// traeCreds extracts access_token and user_id from auth.
func traeCreds(a *cliproxyauth.Auth) (token, userID string) {
	if a == nil {
		return "", ""
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			token = v
		}
		if v, ok := a.Metadata["user_id"].(string); ok {
			userID = v
		}
	}
	if token == "" && a.Attributes != nil {
		if v := a.Attributes["access_token"]; v != "" {
			token = v
		}
		if v := a.Attributes["api_key"]; v != "" {
			token = v
		}
	}
	return
}

// setTraeHeaders sets all required Trae API headers.
func setTraeHeaders(req *http.Request, token, userID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-app-id", traeAppID)
	req.Header.Set("x-ide-version", traeIDEVersion)
	req.Header.Set("x-ide-version-code", traeVersionCode)
	req.Header.Set("x-ide-version-type", traeIDEVersionType)
	req.Header.Set("x-device-cpu", "AMD64 Family 23 Model 113 Stepping 0")
	req.Header.Set("x-device-id", generateStableDeviceID(userID))
	req.Header.Set("x-machine-id", generateStableMachineID(userID))
	req.Header.Set("x-device-brand", traeDeviceBrand)
	req.Header.Set("x-device-type", traeDeviceType)
	if userID != "" {
		req.Header.Set("x-user-id", userID)
	}
	if token != "" {
		req.Header.Set("x-ide-token", token)
	}
}

func generateStableDeviceID(userID string) string {
	if len(userID) >= 4 {
		return fmt.Sprintf("bf0e9a12-%s-4e5f-8a9b-0c1d2e3f4a5b", userID[:4])
	}
	return "bf0e9a12-3c4d-4e5f-8a9b-0c1d2e3f4a5b"
}

func generateStableMachineID(userID string) string {
	if len(userID) >= 12 {
		return fmt.Sprintf("a1b2c3d4-e5f6-7890-abcd-%s", userID[:12])
	}
	return "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// parseTraeSSEResponse reads a full Trae SSE stream and returns the combined content.
func parseTraeSSEResponse(body io.Reader, ctx context.Context, cfg *config.Config) (response, reasoning, finishReason string, err error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, 1_048_576)
	currentEvent := ""

	for scanner.Scan() {
		line := scanner.Bytes()
		helps.AppendAPIResponseChunk(ctx, cfg, line)

		if bytes.HasPrefix(line, []byte("event:")) {
			currentEvent = strings.TrimSpace(string(line[6:]))
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}

		data := bytes.TrimSpace(line[5:])

		if currentEvent == "error" {
			errMsg := extractTraeErrorMessage(data)
			return "", "", "", statusErr{code: http.StatusBadGateway, msg: errMsg}
		}

		if currentEvent == "done" {
			break
		}

		if currentEvent != "output" {
			continue
		}

		var out traeOutputData
		if json.Unmarshal(data, &out) != nil {
			continue
		}
		response += out.Response
		if out.ReasoningContent != "" {
			reasoning += out.ReasoningContent
		}
		if out.FinishReason != "" {
			finishReason = out.FinishReason
		}
	}

	if errScan := scanner.Err(); errScan != nil {
		helps.RecordAPIResponseError(ctx, cfg, errScan)
		return "", "", "", errScan
	}
	return
}

// extractTraeErrorMessage extracts the human-readable message from a Trae error payload.
func extractTraeErrorMessage(data []byte) string {
	msg := gjson.GetBytes(data, "message").String()
	if msg == "" {
		msg = gjson.GetBytes(data, "error").String()
	}
	if msg == "" {
		msg = string(data)
	}
	return msg
}

// extractTraeErrorCode extracts the numeric error code from a Trae error payload.
func extractTraeErrorCode(data []byte) int {
	code := gjson.GetBytes(data, "code").Int()
	if code == 0 {
		code = gjson.GetBytes(data, "status").Int()
	}
	return int(code)
}

// buildTraeRequest converts an OpenAI-format payload body into a Trae request.
func buildTraeRequest(openaiBody []byte, traeModelName string, stream bool) interface{} {
	messages := gjson.GetBytes(openaiBody, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return traeAgentsRunsRequest{
			Messages:  []traeMessage{{Role: "user", Content: ""}},
			ModelName: traeModelName,
			Stream:    stream,
		}
	}

	traeMessages := make([]traeMessage, 0, len(messages.Array()))
	for _, msg := range messages.Array() {
		role := msg.Get("role").String()
		content := msg.Get("content").String()
		if content == "" && msg.Get("content").IsArray() {
			var textParts []string
			msg.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					textParts = append(textParts, part.Get("text").String())
				}
				return true
			})
			content = strings.Join(textParts, "\n")
		}
		if role == "" {
			continue
		}
		traeMessages = append(traeMessages, traeMessage{Role: role, Content: content})
	}

	return traeAgentsRunsRequest{
		Messages:  traeMessages,
		ModelName: traeModelName,
		Stream:    stream,
	}
}

func buildOpenAIChatCompletion(model, content, reasoning, finishReason string, hasReasoning bool) []byte {
	finish := "stop"
	if finishReason == "length" {
		finish = "length"
	}
	resp := map[string]interface{}{
		"id":      "chatcmpl-" + generateUUID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{"index": 0, "message": buildMessage(content, reasoning, hasReasoning), "finish_reason": finish},
		},
		"usage": map[string]interface{}{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

func buildOpenAIStreamChunk(model, content, reasoning, finishReason string, index int, hasReasoning bool) []byte {
	delta := map[string]interface{}{}
	if content != "" {
		delta["content"] = content
	}
	if reasoning != "" && hasReasoning {
		delta["reasoning_content"] = reasoning
	}
	chunk := map[string]interface{}{
		"id":      "chatcmpl-trae",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{"index": index, "delta": delta, "finish_reason": nilIfEmpty(finishReason)},
		},
	}
	b, _ := json.Marshal(chunk)
	return b
}

func buildOpenAIStreamDone() []byte { return []byte("[DONE]") }

func buildMessage(content, reasoning string, hasReasoning bool) map[string]interface{} {
	msg := map[string]interface{}{"role": "assistant", "content": content}
	if reasoning != "" && hasReasoning {
		msg["reasoning_content"] = reasoning
	}
	return msg
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
