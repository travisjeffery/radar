package bedrock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/travisjeffery/radar"
)

func repoTools(t *testing.T) *radar.RepoTools {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "up.go"), []byte("package up\n\nfunc Upload() error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, err := radar.NewRepoTools(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

const readCall = `{"type":"tool_use","id":"tu_read","name":"read_file","input":{"path":"up.go"}}`

func TestAgentReadsTheRepositoryBeforeTheVerdict(t *testing.T) {
	var sent []anthropic.MessageNewParams
	responses := []string{readCall, toolUse(verdictJSON)}
	a := &Agent{Model: "us.anthropic.claude-opus-5-5", MaxTokens: 1000, Effort: anthropic.OutputConfigEffortHigh, Tools: repoTools(t),
		send: func(_ context.Context, p anthropic.MessageNewParams) (anthropic.Message, error) {
			sent = append(sent, p)
			return message(t, "tool_use", responses[len(sent)-1]), nil
		}}
	got := a.review(context.Background(), radar.Diff{Changes: []radar.Change{{File: "README.md", Content: "+x"}}})
	if got.Failed || got.Coverage == nil || !got.Coverage.RepoAccess || got.Coverage.ToolCalls != 1 || got.Usage.Requests != 2 {
		t.Fatalf("review = %q coverage=%+v usage=%+v", got.Summary, got.Coverage, got.Usage)
	}
	// Measurement only: what each tool returned, the first
	// request's split and the shared prefix's hash.
	if use := got.Coverage.Tools["read_file"]; use.Calls != 1 || use.Bytes == 0 || len(got.Coverage.Tools) != 1 {
		t.Fatalf("tools = %+v", got.Coverage.Tools)
	}
	if got.Usage.FirstRequest == nil || len(got.Usage.PrefixSHA256) != 64 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	raw, _ := json.Marshal(sent[1])
	body := string(raw)
	for _, want := range []string{`"name":"read_file"`, `"tool_use_id":"tu_read"`, `func Upload() error`, `"cache_control":{"type":"ephemeral"}`, `read_file, grep, list_dir and git_log`} {
		if !strings.Contains(body, want) {
			t.Fatalf("second request missing %s", want)
		}
	}
	if n := strings.Count(body, `"cache_control"`); n != 2 {
		t.Fatalf("one breakpoint on the system prompt and one on the newest turn, got %d", n)
	}
	if !a.RepoAccess() || a.ReviewVariant() != "repo-tools" {
		t.Fatal("an agent with tools has repository access and its own record key")
	}
}

func TestAgentToolBudgetAsksForTheVerdict(t *testing.T) {
	calls := 0
	a := &Agent{Model: "m", MaxTokens: 1000, Tools: repoTools(t), ToolCallBudget: 1,
		send: func(_ context.Context, p anthropic.MessageNewParams) (anthropic.Message, error) {
			calls++
			if calls <= 2 {
				return message(t, "tool_use", readCall), nil
			}
			raw, _ := json.Marshal(p.Messages[len(p.Messages)-1])
			if !strings.Contains(string(raw), "budget is spent; submit the verdict") {
				t.Fatalf("the model must be told to submit: %s", raw)
			}
			return message(t, "tool_use", toolUse(verdictJSON)), nil
		}}
	got := a.review(context.Background(), radar.Diff{})
	if got.Failed || got.Coverage.ToolCalls != 1 || !strings.Contains(got.Coverage.Limits, "repository tool budget reached (1 calls)") {
		t.Fatalf("coverage = %+v summary=%q", got.Coverage, got.Summary)
	}
}

func TestAgentWithoutToolsOffersOnlyTheVerdict(t *testing.T) {
	a := &Agent{Model: "m", MaxTokens: 1000}
	p := a.params(radar.Diff{}, []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("x"))})
	if len(p.Tools) != 1 || a.RepoAccess() || a.ReviewVariant() != "" {
		t.Fatalf("tools = %d", len(p.Tools))
	}
}

func TestUsageCountsCachedInputAsInput(t *testing.T) {
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(`{"stop_reason":"tool_use","content":[`+toolUse(verdictJSON)+`],"usage":{"input_tokens":8,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200,"output_tokens":50}}`), &msg); err != nil {
		t.Fatal(err)
	}
	res, _ := verdictFrom(msg)
	if u := res.Usage; u.InputTokens != 1208 || u.CacheReadTokens != 1000 || u.CacheWriteTokens != 200 || u.OutputTokens != 50 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestBatchOverTheBudgetIsRecorded(t *testing.T) {
	batch := `{"type":"tool_use","id":"tu_a","name":"read_file","input":{"path":"up.go"}},{"type":"tool_use","id":"tu_b","name":"list_dir","input":{}}`
	calls := 0
	a := &Agent{Model: "m", MaxTokens: 1000, Tools: repoTools(t), ToolCallBudget: 1,
		send: func(_ context.Context, p anthropic.MessageNewParams) (anthropic.Message, error) {
			calls++
			if calls == 1 {
				return message(t, "tool_use", batch), nil
			}
			return message(t, "tool_use", toolUse(verdictJSON)), nil
		}}
	got := a.review(context.Background(), radar.Diff{})
	if got.Coverage.ToolCalls != 1 || !strings.Contains(got.Coverage.Limits, "repository tool budget reached (1 calls)") {
		t.Fatalf("a declined call in a batch must be recorded: %+v", got.Coverage)
	}
}

func TestCorrectionAnswersEveryToolCall(t *testing.T) {
	bad := `{"summary":"","risk":{"level":"low","reason":"r"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":[],"risk":[]},"coverage":{"files_reviewed":["a"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`
	twice := `{"type":"tool_use","id":"tu_1","name":"submit_verdict","input":` + bad + `},{"type":"tool_use","id":"tu_2","name":"submit_verdict","input":` + bad + `}`
	calls := 0
	a := &Agent{Model: "m", MaxTokens: 1000,
		send: func(_ context.Context, p anthropic.MessageNewParams) (anthropic.Message, error) {
			calls++
			if calls == 1 {
				return message(t, "tool_use", twice), nil
			}
			raw, _ := json.Marshal(p.Messages[len(p.Messages)-1])
			if !strings.Contains(string(raw), `"tool_use_id":"tu_1"`) || !strings.Contains(string(raw), `"tool_use_id":"tu_2"`) {
				t.Fatalf("every tool call needs a result: %s", raw)
			}
			return message(t, "tool_use", toolUse(verdictJSON)), nil
		}}
	if got := a.review(context.Background(), radar.Diff{}); got.Failed || calls != 2 {
		t.Fatalf("the correction must succeed: calls=%d %q", calls, got.Summary)
	}
}

func TestToolCallsStopAtTheExplorationDeadline(t *testing.T) {
	calls := 0
	a := &Agent{Model: "m", MaxTokens: 1000, Tools: repoTools(t),
		send: func(_ context.Context, p anthropic.MessageNewParams) (anthropic.Message, error) {
			calls++
			if calls == 1 {
				time.Sleep(700 * time.Millisecond) // past 60% of the 1s deadline
				return message(t, "tool_use", readCall), nil
			}
			return message(t, "tool_use", toolUse(verdictJSON)), nil
		}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := a.review(ctx, radar.Diff{})
	if got.Failed || got.Coverage.ToolCalls != 0 || !strings.Contains(got.Coverage.Limits, "leave time for the verdict") {
		t.Fatalf("coverage = %+v %q", got.Coverage, got.Summary)
	}
}
