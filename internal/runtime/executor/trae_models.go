package executor

import "strings"

const (
	// Trae API configuration.
	traeBaseURL      = "https://a0ai-api-sg.byteintlapi.com"
	traeAppID        = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeIDEVersion   = "3.5.53"
	traeVersionCode  = "20260212"
	traeIDEVersionType = "stable"
	traeDeviceBrand  = "Trae"
	traeDeviceType   = "Windows"

	// Trae API endpoints.
	traeAgentsRunsPath = "/api/ide/v1/agents/runs"
	traeChatPath       = "/api/ide/v1/chat"
)

// traeModelEntry maps an OpenAI-style model name to Trae's internal model_name.
type traeModelEntry struct {
	TraeName  string // internal name used by Trae API
	IsNew     bool   // true → agents/runs, false → /chat
	Reasoning bool   // true → model supports reasoning_content
}

// traeModelMap maps CLI-facing model IDs to Trae's internal model names.
// Keys use "trae-" prefix to avoid collisions with other providers.
// resolveTraeModel also accepts names without the prefix as fallback.
var traeModelMap = map[string]traeModelEntry{
	"trae-gpt-5.4":                {TraeName: "gpt-5.4", IsNew: true, Reasoning: true},
	"trae-gpt-5.3-codex":          {TraeName: "gpt-5.3-codex", IsNew: true, Reasoning: true},
	"trae-gpt-5.2":                {TraeName: "gpt-5.2", IsNew: true, Reasoning: true},
	"trae-deepseek-v3.1":          {TraeName: "deepseek-v3.1", IsNew: true, Reasoning: false},
	"trae-gemini-3.1-pro":         {TraeName: "gemini-3.1-pro", IsNew: true, Reasoning: true},
	"trae-gemini-3-flash-premium": {TraeName: "gemini-3-flash-premium", IsNew: true, Reasoning: true},
	"trae-gemini-2.5-flash":       {TraeName: "gemini_2.5_flash_premium", IsNew: true, Reasoning: false},
	"trae-minimax-m2.7":           {TraeName: "minimax-m2.7", IsNew: true, Reasoning: true},
	"trae-kimi-k2-0905":           {TraeName: "kimi-k2-0905", IsNew: true, Reasoning: false},
}

// resolveTraeModel returns the Trae model entry for the given model ID.
// Tries exact match first, then adds "trae-" prefix, then strips known prefixes.
func resolveTraeModel(modelID string) (traeModelEntry, bool) {
	if entry, ok := traeModelMap[modelID]; ok {
		return entry, true
	}
	// Try with "trae-" prefix for callers that pass the raw model name.
	if entry, ok := traeModelMap["trae-"+modelID]; ok {
		return entry, true
	}
	// Strip any known provider prefixes to find a match.
	stripped := strings.TrimPrefix(modelID, "trae-")
	if stripped != modelID {
		if entry, ok := traeModelMap[modelID]; ok {
			return entry, true
		}
	}
	return traeModelEntry{}, false
}

// traeAgentsRunsRequest is the request body for the /agents/runs endpoint.
type traeAgentsRunsRequest struct {
	Messages  []traeMessage `json:"messages"`
	ModelName string        `json:"model_name"`
	Stream    bool          `json:"stream"`
}

// traeMessage is a single message in the Trae request format.
type traeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// traeSSEEvent represents a parsed SSE event from Trae's streaming response.
type traeSSEEvent struct {
	Event string
	Data  []byte
}

// traeOutputData is the data payload from an "output" SSE event.
type traeOutputData struct {
	Response          string `json:"response"`
	ReasoningContent  string `json:"reasoning_content"`
	FinishReason      string `json:"finish_reason"`
}
