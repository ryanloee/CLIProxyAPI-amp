package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestAggregateCodebuddyStreamBuildsNonStreamResponse(t *testing.T) {
	stream := []byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1770000000,"model":"kimi-k2.5","choices":[{"index":0,"delta":{"role":"assistant","content":"hel","reasoning_content":"think ","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\""}}]},"finish_reason":null}]}

data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1770000000,"model":"kimi-k2.5","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"more","tool_calls":[{"index":0,"function":{"arguments":":\"README.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}

data: [DONE]
`)

	out := aggregateCodebuddyStream(stream, "fallback")

	if got := gjson.GetBytes(out, "id").String(); got != "chatcmpl_1" {
		t.Fatalf("id = %q, want chatcmpl_1; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "kimi-k2.5" {
		t.Fatalf("model = %q, want kimi-k2.5; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "hello" {
		t.Fatalf("content = %q, want hello; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "think more" {
		t.Fatalf("reasoning_content = %q, want think more; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String(); got != `{"path":"README.md"}` {
		t.Fatalf("tool arguments = %q; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.total_tokens").Int(); got != 8 {
		t.Fatalf("total_tokens = %d, want 8; body=%s", got, out)
	}
}

func TestApplyCodebuddyDefaultReasoningUsesOfficialEffort(t *testing.T) {
	out := applyCodebuddyDefaultReasoning([]byte(`{"model":"kimi-k2.5"}`), "kimi-k2.5", "codebuddy")

	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Fatalf("reasoning_effort = %q, want high; body=%s", got, out)
	}
}

func TestApplyCodebuddyDefaultReasoningNormalizesXHigh(t *testing.T) {
	out := applyCodebuddyDefaultReasoning([]byte(`{"reasoning_effort":"xhigh"}`), "kimi-k2.5", "codebuddy")

	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Fatalf("reasoning_effort = %q, want high; body=%s", got, out)
	}
}
