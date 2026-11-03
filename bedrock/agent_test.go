package bedrock

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/travisjeffery/radar"
)

const verdictJSON = `{"summary":"typo fix","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":["doc-comment-update"],"risk":[]},"coverage":{"files_reviewed":["README.md"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`

func message(t *testing.T, stop string, content string) anthropic.Message {
	t.Helper()
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"us.anthropic.claude-sonnet-5","stop_reason":"` + stop + `",
		"usage":{"input_tokens":2255,"output_tokens":246,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},
		"content":[` + content + `]}`
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func toolUse(input string) string {
	return `{"type":"tool_use","id":"tu_1","name":"submit_verdict","input":` + input + `}`
}

func TestVerdictFrom(t *testing.T) {
	tests := []struct {
		name      string
		stop      string
		content   string
		wantErr   string
		truncated bool
	}{
		{name: "verdict", stop: "tool_use", content: `{"type":"thinking","thinking":"","signature":"s"},` + toolUse(verdictJSON)},
		{name: "truncated", stop: "max_tokens", content: `{"type":"thinking","thinking":"","signature":"s"}`, wantErr: "truncated", truncated: true},
		{name: "refusal", stop: "refusal", content: `{"type":"text","text":""}`, wantErr: "refused"},
		{name: "no tool call", stop: "end_turn", content: `{"type":"text","text":"looks fine"}`, wantErr: "expected one submit_verdict call, got 0"},
		{name: "two tool calls", stop: "tool_use", content: toolUse(verdictJSON) + "," + toolUse(verdictJSON), wantErr: "got 2"},
		{name: "verdict wrapped in verdict", stop: "tool_use", content: toolUse(`{"verdict":` + verdictJSON + `}`)},
		{name: "verdict wrapped in input", stop: "tool_use", content: toolUse(`{"input":` + verdictJSON + `}`)},
		{name: "wrapper beside other keys stays strict", stop: "tool_use", content: toolUse(`{"verdict":` + verdictJSON + `,"x":1}`), wantErr: "parsing verdict"},
		{name: "findings as a JSON string", stop: "tool_use", content: toolUse(`{"summary":"typo fix","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":"[]","questions":[],"tests_suggested":[],"signals":{"safe":["doc-comment-update"],"risk":[]},"coverage":{"files_reviewed":["README.md"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`)},
		{name: "summary that looks like an array stays a string", stop: "tool_use", content: toolUse(`{"summary":"[x]","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":["doc-comment-update"],"risk":[]},"coverage":{"files_reviewed":["README.md"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`)},
		{name: "unknown field", stop: "tool_use", content: toolUse(`{"summary":"s","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":[],"risk":[]},"coverage":{"files_reviewed":["a"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9,"extra":1}`), wantErr: "parsing verdict"},
		{name: "unknown signal", stop: "tool_use", content: toolUse(`{"summary":"s","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":[],"risk":["vibes"]},"coverage":{"files_reviewed":["a"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`), wantErr: "unknown risk signal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := verdictFrom(message(t, tt.stop, tt.content))
			if got.Usage == nil || got.Usage.InputTokens != 2255 || got.Usage.OutputTokens != 246 || got.Usage.Requests != 1 {
				t.Fatalf("usage = %+v", got.Usage)
			}
			if got.Truncated != tt.truncated {
				t.Fatalf("truncated = %t, want %t", got.Truncated, tt.truncated)
			}
			if tt.wantErr == "" {
				if !got.Accept || got.Confidence != 9 || len(got.ReviewedFiles) != 1 {
					t.Fatalf("got %+v", got)
				}
				return
			}
			if got.Accept || got.Confidence != 0 || !strings.Contains(got.Summary, tt.wantErr) {
				t.Fatalf("must fail safe with %q, got %+v", tt.wantErr, got)
			}
		})
	}
}

func TestParamsAvoidFieldsBedrockRejects(t *testing.T) {
	a := &Agent{Model: "us.anthropic.claude-sonnet-5", MaxTokens: defaultMaxTokens, Effort: anthropic.OutputConfigEffortHigh}
	d := radar.Diff{Changes: []radar.Change{{File: "a", Content: "+x"}}}
	raw, err := json.Marshal(a.params(d, []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(radar.RenderDiffForReview(d)))}))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	cfg, _ := body["output_config"].(map[string]any)
	if _, has := cfg["format"]; has {
		t.Fatal("Bedrock rejects output_config.format for Claude")
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if _, has := tool["strict"]; has {
		t.Fatal("Bedrock rejects strict tools for Claude")
	}
	if choice := body["tool_choice"].(map[string]any)["type"]; choice != "auto" {
		t.Fatalf("tool_choice = %v; forced choice is incompatible with thinking", choice)
	}
	if body["thinking"].(map[string]any)["type"] != "adaptive" {
		t.Fatalf("thinking = %v", body["thinking"])
	}
	schema := tool["input_schema"].(map[string]any)
	if schema["additionalProperties"] != false || len(schema["required"].([]any)) != 11 {
		t.Fatalf("input schema = %v", schema)
	}
}

func TestReviewRetriesOnceOnMalformedVerdict(t *testing.T) {
	bad := toolUse(`{"summary":"s","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[{"severity":"P2","title":"","file":"","line":0,"end_line":0,"impact":"","recommendation":"","rule_id":"","confidence":8}],"questions":[],"tests_suggested":[],"signals":{"safe":[],"risk":[]},"coverage":{"files_reviewed":["a"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`)
	tests := []struct {
		name      string
		responses []string
		stops     []string
		wantCalls int
		wantPass  bool
	}{
		{name: "second answer is good", responses: []string{bad, toolUse(verdictJSON)}, stops: []string{"tool_use", "tool_use"}, wantCalls: 2, wantPass: true},
		{name: "two bad answers fail safe", responses: []string{bad, bad}, stops: []string{"tool_use", "tool_use"}, wantCalls: 2},
		{name: "truncation is not retried", responses: []string{`{"type":"text","text":"x"}`}, stops: []string{"max_tokens"}, wantCalls: 1},
		{name: "refusal is not retried", responses: []string{`{"type":"text","text":""}`}, stops: []string{"refusal"}, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			a := &Agent{Model: "m", MaxTokens: 1000, send: func(context.Context, anthropic.MessageNewParams) (anthropic.Message, error) {
				i := min(calls, len(tt.responses)-1)
				calls++
				return message(t, tt.stops[i], tt.responses[i]), nil
			}}
			got := a.review(context.Background(), radar.Diff{})
			if calls != tt.wantCalls || got.Accept != tt.wantPass || got.Usage == nil || got.Usage.Requests != calls {
				t.Fatalf("calls=%d accept=%t usage=%+v summary=%q", calls, got.Accept, got.Usage, got.Summary)
			}
			if !tt.wantPass && tt.wantCalls == 2 && !strings.Contains(got.Summary, "input {") {
				t.Fatalf("a malformed verdict must be recorded for diagnosis: %q", got.Summary)
			}
		})
	}
}

func TestAgentDeclaresConcurrencySafety(t *testing.T) {
	var agent radar.ReviewAgent = &Agent{}
	c, ok := agent.(radar.ConcurrentReviewAgent)
	if !ok || !c.ConcurrentReviewSafe() {
		t.Fatal("the bedrock agent must declare ConcurrentReviewSafe so large diffs are reviewed in parallel")
	}
}

// The tool instruction lists the review's fields by name; a field missing
// from it is one Claude may never send.
func TestToolInstructionNamesEverySchemaField(t *testing.T) {
	for field := range radar.ACRVerdictSchema()["properties"].(map[string]any) {
		if !strings.Contains(toolInstruction, field) {
			t.Errorf("toolInstruction does not name %q", field)
		}
	}
}
