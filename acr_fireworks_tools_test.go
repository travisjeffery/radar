package radar

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const toolTestReview = `{"summary":"adds retry","risk":{"level":"low","reason":"small"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":["defensive-programming"],"risk":[]},"coverage":{"files_reviewed":["svc/up.go"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`

// fakeToolServer answers exploration requests with a read_file call until
// it has seen `calls` tool results (or forever, if always), then stops
// exploring; a request with a response_format gets the review.
func fakeToolServer(t *testing.T, calls int, always bool) (*httptest.Server, *[]fireworksChatReq) {
	var mu sync.Mutex
	var seen []fireworksChatReq
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req fireworksChatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(s string) { _, _ = io.WriteString(w, "data: "+s+"\n\n") }
		usage := `{"model":"` + fireworksTestModel + `","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`
		toolResults := 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults++
			}
		}
		switch {
		case req.ResponseFormat != nil:
			quoted, _ := json.Marshal(toolTestReview)
			send(`{"model":"` + fireworksTestModel + `","choices":[{"delta":{"content":` + string(quoted) + `},"finish_reason":"stop"}]}`)
		case always || toolResults < calls:
			// The arguments arrive in two fragments, as Fireworks streams them.
			send(`{"model":"` + fireworksTestModel + `","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`)
			send(`{"model":"` + fireworksTestModel + `","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"svc/up.go\"}"}}]},"finish_reason":"tool_calls"}]}`)
		default:
			send(`{"model":"` + fireworksTestModel + `","choices":[{"delta":{"content":"I have what I need."},"finish_reason":"stop"}]}`)
		}
		send(usage)
		send("[DONE]")
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func TestFireworksAgentReadsTheRepository(t *testing.T) {
	tools, _ := toolsRepo(t)
	server, seen := fakeToolServer(t, 1, false)
	agent := &FireworksAgent{APIKey: "k", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client(), Tools: tools}
	got := agent.Review(Diff{Changes: []Change{{File: "svc/up.go", Content: "+retry"}}})
	if got.Failed || got.Coverage == nil || !got.Coverage.RepoAccess || got.Coverage.ToolCalls != 1 || got.Usage.Requests != 3 {
		t.Fatalf("review = %+v coverage=%+v usage=%+v", got.Summary, got.Coverage, got.Usage)
	}
	reqs := *seen
	if len(reqs[0].Tools) != 4 || reqs[0].ResponseFormat != nil || !strings.Contains(reqs[0].Messages[0].Content, "read_file, grep, list_dir and git_log") {
		t.Fatalf("exploration request = %+v", reqs[0])
	}
	second := reqs[1].Messages
	tool := second[len(second)-1]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || !strings.Contains(tool.Content, "func Upload() error") || second[len(second)-2].ToolCalls[0].Function.Arguments != `{"path":"svc/up.go"}` {
		t.Fatalf("tool round trip = %+v", second[len(second)-2:])
	}
	if reqs[0].ReasoningEffort != "low" || reqs[0].MaxTokens != exploreMaxTokens {
		t.Fatalf("exploration rounds are light: effort %q max_tokens %d", reqs[0].ReasoningEffort, reqs[0].MaxTokens)
	}
	if last := reqs[2]; last.ResponseFormat == nil || len(last.Tools) != 0 || last.ReasoningEffort != "" || last.MaxTokens != defaultFireworksMaxTokens {
		t.Fatalf("the review is asked for under the schema without tools: %+v", last)
	}
}

func TestFireworksToolBudget(t *testing.T) {
	tools, _ := toolsRepo(t)
	server, seen := fakeToolServer(t, 0, true)
	agent := &FireworksAgent{APIKey: "k", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client(), Tools: tools, ToolCallBudget: 2}
	got := agent.Review(Diff{Changes: []Change{{File: "svc/up.go", Content: "+retry"}}})
	if got.Failed || got.Coverage.ToolCalls != 2 || !strings.Contains(got.Coverage.Limits, "repository tool budget reached (2 calls)") {
		t.Fatalf("coverage = %+v summary=%q", got.Coverage, got.Summary)
	}
	if n := len(*seen); n != 3 {
		t.Fatalf("two exploration rounds and the review, got %d requests", n)
	}

	server, _ = fakeToolServer(t, 0, true)
	agent = &FireworksAgent{APIKey: "k", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client(), Tools: tools, ToolTokenBudget: 1500}
	if got := agent.Review(Diff{Changes: []Change{{File: "svc/up.go", Content: "+retry"}}}); !strings.Contains(got.Coverage.Limits, "token budget reached") {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
}

func TestFireworksWithoutToolsHasNoRepoAccess(t *testing.T) {
	server, seen := fakeToolServer(t, 0, false)
	agent := &FireworksAgent{APIKey: "k", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client()}
	got := agent.Review(Diff{Changes: []Change{{File: "svc/up.go", Content: "+retry"}}})
	if got.Failed || agent.RepoAccess() || len(*seen) != 1 || len((*seen)[0].Tools) != 0 {
		t.Fatalf("without a checkout there is one request and no tools: %d requests, %+v", len(*seen), got.Summary)
	}
}

// A review written in a low-effort exploration round is never the review.
func TestExplorationAnswerIsNotTheReview(t *testing.T) {
	tools, _ := toolsRepo(t)
	var mu sync.Mutex
	var requests []fireworksChatReq
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req fireworksChatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		review := toolTestReview
		if req.ResponseFormat == nil {
			// The exploration round answers with a review that blocks nothing.
			review = strings.Replace(toolTestReview, `"summary":"adds retry"`, `"summary":"low effort"`, 1)
		} else {
			review = strings.Replace(toolTestReview, `"findings":[]`, `"findings":[{"severity":"P1","title":"t","file":"svc/up.go","line":1,"end_line":1,"impact":"i","recommendation":"r","rule_id":"","confidence":9}]`, 1)
		}
		quoted, _ := json.Marshal(review)
		_, _ = io.WriteString(w, "data: "+`{"model":"`+fireworksTestModel+`","choices":[{"delta":{"content":`+string(quoted)+`},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	agent := &FireworksAgent{APIKey: "k", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client(), Tools: tools}
	got := agent.Review(Diff{Changes: []Change{{File: "svc/up.go", Content: "+retry"}}})
	if got.Summary != "adds retry" || len(got.Findings) != 1 || len(requests) != 2 || requests[1].ResponseFormat == nil {
		t.Fatalf("the review must come from the full-effort request: %q findings=%d requests=%d", got.Summary, len(got.Findings), len(requests))
	}
}

func TestToolModeIsPartOfTheRecordedKey(t *testing.T) {
	tools, _ := toolsRepo(t)
	d := Diff{Changes: []Change{{File: "svc/up.go", Content: "+retry"}}}
	memo := &reviewMemo{byKey: map[string]ACRResult{}}
	diffOnly := &FireworksAgent{Model: fireworksTestModel}
	withTools := &FireworksAgent{Model: fireworksTestModel, Tools: tools}
	stub := func(a *FireworksAgent) memoAgent {
		return memoAgent{agent: variantStub{a}, name: a.Describe(), memo: memo}
	}
	stub(diffOnly).Review(d)
	if _, ok := memo.lookup(RequestKey(withTools.Describe()+"\x00repo-tools", d)); ok {
		t.Fatal("a diff-only review must not answer a tool-enabled request")
	}
	stub(withTools).Review(d)
	if len(memo.order) != 2 {
		t.Fatalf("the two modes must be recorded apart: %d", len(memo.order))
	}
}

// variantStub answers like the agent's variant without calling Fireworks.
type variantStub struct{ a *FireworksAgent }

func (v variantStub) Review(d Diff) ACRResult {
	return ACRResult{ReviewedFiles: []string{"svc/up.go"}, Summary: "ok"}
}
func (v variantStub) ReviewVariant() string { return v.a.ReviewVariant() }

func TestSlowExplorationRoundKeepsTheReviewTime(t *testing.T) {
	tools, _ := toolsRepo(t)
	release := make(chan struct{})
	defer close(release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req fireworksChatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ResponseFormat == nil {
			select { // an exploration round that never finishes
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		quoted, _ := json.Marshal(toolTestReview)
		_, _ = io.WriteString(w, "data: "+`{"model":"`+fireworksTestModel+`","choices":[{"delta":{"content":`+string(quoted)+`},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	agent := &FireworksAgent{APIKey: "k", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client(), Tools: tools, Timeout: 2 * time.Second}
	got := agent.Review(Diff{Changes: []Change{{File: "svc/up.go", Content: "+retry"}}})
	if got.Failed || !strings.Contains(got.Coverage.Limits, "to leave time for the review") {
		t.Fatalf("a slow round must end exploration, not the review: %q coverage=%+v", got.Summary, got.Coverage)
	}
}

func TestFireworksUsageRecordsCachedTokens(t *testing.T) {
	var u fireworksUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800}}`), &u); err != nil {
		t.Fatal(err)
	}
	got := addFireworksUsage(nil, &u)
	if got.InputTokens != 1000 || got.CacheReadTokens != 800 || got.OutputTokens != 50 || got.Requests != 1 {
		t.Fatalf("usage = %+v", got)
	}
}
