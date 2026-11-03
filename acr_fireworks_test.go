package radar

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fireworksTestModel = "accounts/fireworks/models/glm-5p3"

func TestNewFireworksAgentFromEnvironment(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		model     string
		maxTokens string
		wantErr   string
	}{
		{name: "missing key", model: fireworksTestModel, wantErr: "FIREWORKS_API_KEY"},
		{name: "missing model", key: "test-key", wantErr: "RADAR_ACR_MODEL"},
		{name: "blank model", key: "test-key", model: "  ", wantErr: "RADAR_ACR_MODEL"},
		{name: "invalid max tokens", key: "test-key", model: fireworksTestModel, maxTokens: "lots", wantErr: "RADAR_ACR_MAX_TOKENS"},
		{name: "zero max tokens", key: "test-key", model: fireworksTestModel, maxTokens: "0", wantErr: "RADAR_ACR_MAX_TOKENS"},
		{name: "ok", key: "test-key", model: fireworksTestModel},
		{name: "ok with max tokens", key: "test-key", model: fireworksTestModel, maxTokens: "4096"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FIREWORKS_API_KEY", tt.key)
			t.Setenv("RADAR_ACR_MODEL", tt.model)
			t.Setenv("RADAR_ACR_MAX_TOKENS", tt.maxTokens)
			t.Setenv("FIREWORKS_BASE_URL", "")
			agent, err := NewFireworksAgent()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if agent.Model != fireworksTestModel || agent.BaseURL != fireworksChatCompletionsURL {
				t.Fatalf("unexpected agent: %+v", agent)
			}
			wantMax := defaultFireworksMaxTokens
			if tt.maxTokens != "" {
				wantMax = 4096
			}
			if agent.MaxTokens != wantMax {
				t.Fatalf("max tokens = %d, want %d", agent.MaxTokens, wantMax)
			}
			if agent.Describe() != "fireworks/"+fireworksTestModel {
				t.Fatalf("describe = %q", agent.Describe())
			}
		})
	}
}

func TestNewFireworksAgentHonoursBaseURLOverride(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "test-key")
	t.Setenv("RADAR_ACR_MODEL", fireworksTestModel)
	t.Setenv("FIREWORKS_BASE_URL", "http://127.0.0.1:9/v1/chat/completions")
	agent, err := NewFireworksAgent()
	if err != nil {
		t.Fatal(err)
	}
	if agent.BaseURL != "http://127.0.0.1:9/v1/chat/completions" {
		t.Fatalf("base URL = %q", agent.BaseURL)
	}
}

func TestFireworksAgentReviewUsesChatCompletions(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/inference/v1/chat/completions" {
			t.Errorf("path = %q, want /inference/v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q, want application/json", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"cmpl-1","model":"` + fireworksTestModel + `",
			"choices":[{"index":0,"finish_reason":"stop","message":{
				"role":"assistant",
				"reasoning_content":"docs only, nothing risky",
				"content":"{\"summary\":\"documentation only\",\"risk\":{\"level\":\"low\",\"reason\":\"test\"},\"where_to_look\":[],\"findings\":[],\"questions\":[],\"tests_suggested\":[],\"signals\":{\"safe\":[\"doc-comment-update\"],\"risk\":[]},\"coverage\":{\"files_reviewed\":[\"README.md\"],\"repo_access\":false,\"limits\":\"\"},\"needs_human\":{\"required\":false,\"reason\":\"\"},\"confidence\":10}"
			}}],
			"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}
		}`))
	}))
	defer server.Close()

	agent := &FireworksAgent{
		APIKey:    "test-key",
		Model:     fireworksTestModel,
		BaseURL:   server.URL + "/inference/v1/chat/completions",
		MaxTokens: 4096,
		HTTP:      server.Client(),
	}
	result := agent.Review(Diff{
		ID:     "pr-123",
		Org:    "example",
		Source: SourceHuman,
		Changes: []Change{{
			File:       "README.md",
			Content:    "+clarify setup",
			Complexity: 1,
		}},
	})
	if !result.Accept || result.Confidence != 10 || result.Summary != "documentation only" {
		t.Fatalf("unexpected result: %+v", result)
	}

	if request["model"] != fireworksTestModel {
		t.Fatalf("model = %#v", request["model"])
	}
	if request["temperature"] != float64(0) {
		t.Fatalf("temperature = %#v, want 0", request["temperature"])
	}
	if request["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %#v, want 4096", request["max_tokens"])
	}
	messages := request["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want system + user", len(messages))
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" || !strings.Contains(system["content"].(string), "Respond with ONLY a JSON object") {
		t.Fatalf("system message = %#v", system)
	}
	user := messages[1].(map[string]any)
	if user["role"] != "user" || !strings.Contains(user["content"].(string), "README.md") {
		t.Fatalf("user message = %#v", user)
	}

	format := request["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("response_format type = %#v", format["type"])
	}
	jsonSchema := format["json_schema"].(map[string]any)
	if jsonSchema["name"] != "radar_acr_verdict" {
		t.Fatalf("json_schema name = %#v", jsonSchema["name"])
	}
	schema := jsonSchema["schema"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Fatal("root schema must reject additional properties")
	}
	if properties := schema["properties"].(map[string]any); len(properties) != 11 {
		t.Fatalf("schema has %d properties, want 11", len(properties))
	}
}

func TestFireworksAgentStripsInlineThinkBlock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{
			"content":"<think>\nweighing the change\n</think>\n{\"summary\":\"reshapes the API\",\"risk\":{\"level\":\"low\",\"reason\":\"test\"},\"where_to_look\":[],\"findings\":[],\"questions\":[],\"tests_suggested\":[],\"signals\":{\"safe\":[],\"risk\":[\"structural-change\"]},\"coverage\":{\"files_reviewed\":[\"src/a.go\"],\"repo_access\":false,\"limits\":\"\"},\"needs_human\":{\"required\":false,\"reason\":\"\"},\"confidence\":4}"
		}}]}`))
	}))
	defer server.Close()

	agent := &FireworksAgent{APIKey: "test-key", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client()}
	result := agent.Review(Diff{Changes: []Change{{File: "src/a.go", Content: "-x\n+y"}}})
	if result.Accept || result.Confidence != 4 || result.Summary != "reshapes the API" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.RiskSignals) != 1 || result.RiskSignals[0] != SignalStructuralChange {
		t.Fatalf("risk signals = %v", result.RiskSignals)
	}
}

func TestStripThinkBlock(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"<think>hmm</think>{\"a\":1}", `{"a":1}`},
		{"  <think>\nmulti\nline\n</think>\n\n{\"a\":1}\n", `{"a":1}`},
		{"<think>never closed {\"a\":1}", "<think>never closed {\"a\":1}"},
		{"prefix <think>x</think>{\"a\":1}", "prefix <think>x</think>{\"a\":1}"},
	}
	for _, tt := range tests {
		if got := stripThinkBlock(tt.in); got != tt.want {
			t.Errorf("stripThinkBlock(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFireworksAgentFailsSafe(t *testing.T) {
	verdict := `{\"summary\":\"ok\",\"risk\":{\"level\":\"low\",\"reason\":\"test\"},\"where_to_look\":[],\"findings\":[],\"questions\":[],\"tests_suggested\":[],\"signals\":{\"safe\":[\"pure-formatting\"],\"risk\":[]},\"coverage\":{\"files_reviewed\":[\"a.go\"],\"repo_access\":false,\"limits\":\"\"},\"needs_human\":{\"required\":false,\"reason\":\"\"},\"confidence\":10}`
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{
			name:       "API error status",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":{"message":"denied"}}`,
			want:       "fireworks API status 401",
		},
		{
			name:       "error object with 200",
			statusCode: http.StatusOK,
			body:       `{"error":{"message":"model overloaded"}}`,
			want:       "fireworks API error: model overloaded",
		},
		{
			name:       "no choices",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[]}`,
			want:       "no choices",
		},
		{
			name:       "truncated at max_tokens",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"length","message":{"content":"{\"accept\":tr"}}]}`,
			want:       "truncated at max_tokens",
		},
		{
			name:       "unexpected finish reason",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"content_filter","message":{"content":"` + verdict + `"}}]}`,
			want:       `finish_reason "content_filter"`,
		},
		{
			name:       "served a different model",
			statusCode: http.StatusOK,
			body:       `{"model":"accounts/fireworks/models/other","choices":[{"finish_reason":"stop","message":{"content":"` + verdict + `"}}]}`,
			want:       "served model",
		},
		{
			name:       "served model omitted",
			statusCode: http.StatusOK,
			body:       `{"choices":[{"finish_reason":"stop","message":{"content":"` + verdict + `"}}]}`,
			want:       `served model "", want`,
		},
		{
			name:       "finish reason omitted",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"message":{"content":"` + verdict + `"}}]}`,
			want:       `finish_reason ""`,
		},
		{
			name:       "refusal",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{"content":"","refusal":"cannot review"}}]}`,
			want:       "refused review",
		},
		{
			name:       "empty content",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{"content":"  "}}]}`,
			want:       "no content",
		},
		{
			name:       "malformed verdict",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{"content":"not json"}}]}`,
			want:       "invalid character",
		},
		{
			name:       "unknown signal",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"ok\",\"risk\":{\"level\":\"low\",\"reason\":\"test\"},\"where_to_look\":[],\"findings\":[],\"questions\":[],\"tests_suggested\":[],\"signals\":{\"safe\":[\"looks-fine\"],\"risk\":[]},\"coverage\":{\"files_reviewed\":[\"a.go\"],\"repo_access\":false,\"limits\":\"\"},\"needs_human\":{\"required\":false,\"reason\":\"\"},\"confidence\":10}"}}]}`,
			want:       `unknown safe signal "looks-fine"`,
		},
		{
			name:       "malformed response body",
			statusCode: http.StatusOK,
			body:       `{`,
			want:       "unexpected end of JSON input",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			agent := &FireworksAgent{APIKey: "test-key", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client()}
			result := agent.Review(Diff{})
			if result.Accept || result.Confidence != 0 {
				t.Fatalf("error must fail safe: %+v", result)
			}
			if !strings.Contains(result.Summary, tt.want) {
				t.Fatalf("summary = %q, want substring %q", result.Summary, tt.want)
			}
		})
	}
}

func TestFireworksAgentFailsSafeWithoutModel(t *testing.T) {
	agent := &FireworksAgent{APIKey: "test-key", BaseURL: "http://127.0.0.1:9"}
	result := agent.Review(Diff{})
	if result.Accept || !strings.Contains(result.Summary, "model is required") {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestFireworksAgentFailsSafeOnTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{}`))
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	agent := &FireworksAgent{
		APIKey:  "test-key",
		Model:   fireworksTestModel,
		BaseURL: server.URL,
		HTTP:    &http.Client{Timeout: 50 * time.Millisecond},
	}
	result := agent.Review(Diff{})
	if result.Accept || result.Confidence != 0 || !result.Failed || !strings.Contains(result.Summary, "failing safe") {
		t.Fatalf("timeout must fail safe: %+v", result)
	}
}

func TestNewFireworksAgentTimeout(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: defaultFireworksTimeout},
		{name: "override", raw: "90s", want: 90 * time.Second},
		{name: "minutes", raw: "5m", want: 5 * time.Minute},
		{name: "not a duration", raw: "240", wantErr: true},
		{name: "zero", raw: "0s", wantErr: true},
		{name: "negative", raw: "-1s", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FIREWORKS_API_KEY", "test-key")
			t.Setenv("RADAR_ACR_MODEL", fireworksTestModel)
			t.Setenv("RADAR_ACR_TIMEOUT", tt.raw)
			agent, err := NewFireworksAgent()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "RADAR_ACR_TIMEOUT") {
					t.Fatalf("err = %v, want RADAR_ACR_TIMEOUT error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if agent.Timeout != tt.want || agent.HTTP.Timeout != tt.want {
				t.Fatalf("timeout = %v / http %v, want %v", agent.Timeout, agent.HTTP.Timeout, tt.want)
			}
		})
	}
}

func TestFireworksAgentRecordsElapsed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"needs a human\",\"risk\":{\"level\":\"low\",\"reason\":\"test\"},\"where_to_look\":[],\"findings\":[],\"questions\":[],\"tests_suggested\":[],\"signals\":{\"safe\":[],\"risk\":[]},\"coverage\":{\"files_reviewed\":[\"a.go\"],\"repo_access\":false,\"limits\":\"\"},\"needs_human\":{\"required\":false,\"reason\":\"\"},\"confidence\":5}"}}]}`))
	}))
	defer server.Close()
	agent := &FireworksAgent{APIKey: "test-key", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client()}
	result := agent.Review(Diff{Changes: []Change{{File: "a.go"}}})
	if result.Summary != "needs a human" || result.ElapsedMS == nil || *result.ElapsedMS < 20 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestFireworksAgentTimeoutFailsSafeAndRecordsElapsed(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	agent := &FireworksAgent{APIKey: "test-key", Model: fireworksTestModel, BaseURL: server.URL, Timeout: 50 * time.Millisecond, HTTP: &http.Client{}}
	result := agent.Review(Diff{})
	if result.Accept || !strings.Contains(result.Summary, "failing safe") || result.ElapsedMS == nil || *result.ElapsedMS < 50 {
		t.Fatalf("deadline must fail safe and record elapsed: %+v", result)
	}
}

func TestFireworksAgentEmitsElapsedEvenWhenZero(t *testing.T) {
	agent := &FireworksAgent{APIKey: "test-key"}
	result := agent.Review(Diff{})
	if result.Accept || result.ElapsedMS == nil {
		t.Fatalf("an immediate failure must still record elapsed: %+v", result)
	}
	out, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"elapsed_ms":`) {
		t.Fatalf("elapsed_ms missing from %s", out)
	}
	rule, _ := json.Marshal(RuleBasedAgent{}.Review(Diff{}))
	if strings.Contains(string(rule), "elapsed_ms") {
		t.Fatalf("agents that do not measure latency must omit elapsed_ms: %s", rule)
	}
}

func TestFireworksAgentReportsTruncationAndUsage(t *testing.T) {
	body := `{"model":"` + fireworksTestModel + `","usage":{"prompt_tokens":1200,"completion_tokens":24576},"choices":[{"finish_reason":"length","message":{"content":"{\"accept\":tr"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer server.Close()
	agent := &FireworksAgent{APIKey: "test-key", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client()}
	got := agent.Review(Diff{})
	if !got.Truncated || got.Usage == nil || got.Usage.OutputTokens != 24576 || got.Usage.InputTokens != 1200 {
		t.Fatalf("truncation and usage not reported: %+v usage=%+v", got, got.Usage)
	}

	body = `{"model":"` + fireworksTestModel + `","usage":{"prompt_tokens":10,"completion_tokens":5},"choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"s\",\"risk\":{\"level\":\"low\",\"reason\":\"test\"},\"where_to_look\":[],\"findings\":[],\"questions\":[],\"tests_suggested\":[],\"signals\":{\"safe\":[],\"risk\":[]},\"coverage\":{\"files_reviewed\":[\"a\"],\"repo_access\":false,\"limits\":\"\"},\"needs_human\":{\"required\":false,\"reason\":\"\"},\"confidence\":4}"}}]}`
	got = agent.Review(Diff{})
	if got.Truncated || got.Usage == nil || got.Usage.Requests != 1 || got.Usage.OutputTokens != 5 {
		t.Fatalf("usage not recorded on success: %+v usage=%+v", got, got.Usage)
	}
}

func TestFireworksAgentStreamsTheReview(t *testing.T) {
	review := `{"summary":"docs","risk":{"level":"low","reason":"docs"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":["doc-comment-update"],"risk":[]},"coverage":{"files_reviewed":["README.md"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`
	quoted, _ := json.Marshal(review)
	half := len(quoted) / 2
	var sent map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(s string) { _, _ = io.WriteString(w, "data: "+s+"\n\n") }
		chunk(`{"model":"` + fireworksTestModel + `","choices":[{"delta":{"content":` + string(quoted[:half]) + `"}}]}`)
		chunk(`{"model":"` + fireworksTestModel + `","choices":[{"delta":{"content":"` + string(quoted[half:]) + `},"finish_reason":"stop"}]}`)
		chunk(`{"model":"` + fireworksTestModel + `","choices":[],"usage":{"prompt_tokens":900,"completion_tokens":120}}`)
		chunk(`[DONE]`)
	}))
	defer server.Close()
	agent := &FireworksAgent{APIKey: "k", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client()}
	got := agent.Review(Diff{Changes: []Change{{File: "README.md", Content: "+docs"}}})
	if got.Failed || got.Summary != "docs" || got.Confidence != 9 || got.Usage == nil || got.Usage.OutputTokens != 120 {
		t.Fatalf("streamed review = %+v", got)
	}
	if sent["stream"] != true {
		t.Fatalf("request must stream: %v", sent["stream"])
	}
}

func TestFireworksStreamFailsSafe(t *testing.T) {
	for name, body := range map[string]string{
		"truncated":   `data: {"model":"` + fireworksTestModel + `","choices":[{"delta":{"content":"{\"summ"},"finish_reason":"length"}]}` + "\n\ndata: [DONE]\n\n",
		"error chunk": `data: {"error":{"message":"overloaded"}}` + "\n\n",
		"cut off":     `data: {"model":"` + fireworksTestModel + `","choices":[{"delta":{"content":"{"}}]}` + "\n\n",
		"wrong model": `data: {"model":"other","choices":[{"delta":{"content":"{}"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			agent := &FireworksAgent{APIKey: "k", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client()}
			got := agent.Review(Diff{Changes: []Change{{File: "a.go", Content: "+x"}}})
			if !got.Failed || got.Accept {
				t.Fatalf("must fail safe: %+v", got)
			}
			if name == "truncated" && !got.Truncated {
				t.Fatal("a stream that stops at max_tokens is truncated, so Radar splits it")
			}
		})
	}
}
