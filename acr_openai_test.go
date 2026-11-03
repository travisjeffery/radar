package radar

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewOpenAIAgentUsesResponsesAPI(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("RADAR_ACR_MODEL", "test-model")

	agent, err := NewOpenAIAgent()
	if err != nil {
		t.Fatal(err)
	}
	if agent.Model != "test-model" {
		t.Fatalf("model = %q, want test-model", agent.Model)
	}
	if agent.BaseURL != "https://api.openai.com/v1/responses" {
		t.Fatalf("base URL = %q, want Responses endpoint", agent.BaseURL)
	}
}

func TestOpenAIAgentReviewUsesResponsesAPI(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", r.URL.Path)
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
			"status":"completed",
			"output":[
				{"type":"reasoning","content":[]},
				{"type":"message","content":[
					{"type":"output_text","text":"{\"summary\":\"documentation only\",\"risk\":{\"level\":\"low\",\"reason\":\"docs\"},\"where_to_look\":[],\"findings\":[],"},
					{"type":"output_text","text":"\"questions\":[],\"tests_suggested\":[],\"signals\":{\"safe\":[\"doc-comment-update\"],\"risk\":[]},\"coverage\":{\"files_reviewed\":[\"README.md\"],\"repo_access\":false,\"limits\":\"\"},\"needs_human\":{\"required\":false,\"reason\":\"\"},\"confidence\":10}"}
				]}
			]
		}`))
	}))
	defer server.Close()

	agent := &OpenAIAgent{
		APIKey:  "test-key",
		Model:   "gpt-4o-mini",
		BaseURL: server.URL + "/v1/responses",
		HTTP:    server.Client(),
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

	if request["model"] != "gpt-4o-mini" {
		t.Fatalf("model = %#v", request["model"])
	}
	if request["store"] != false {
		t.Fatalf("store = %#v, want explicit false", request["store"])
	}
	if _, ok := request["max_output_tokens"]; ok {
		t.Fatal("endpoint migration should preserve the existing output-token behavior")
	}
	if !strings.Contains(request["instructions"].(string), "Respond with ONLY a JSON object") {
		t.Fatal("system instructions missing strict JSON requirement")
	}
	if !strings.Contains(request["input"].(string), "README.md") {
		t.Fatal("input missing changed file")
	}

	text := request["text"].(map[string]any)
	format := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "radar_acr_verdict" || format["strict"] != true {
		t.Fatalf("unexpected structured-output format: %#v", format)
	}
	schema := format["schema"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Fatal("root schema must reject additional properties")
	}
	properties := schema["properties"].(map[string]any)
	if len(properties) != 11 {
		t.Fatalf("schema has %d properties, want 11", len(properties))
	}
	findings := properties["findings"].(map[string]any)
	findingItems := findings["items"].(map[string]any)
	if findingItems["additionalProperties"] != false {
		t.Fatal("finding schema must reject additional properties")
	}
}

func TestOpenAIAgentFailsSafeOnResponsesErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{
			name:       "incomplete response",
			statusCode: http.StatusOK,
			body:       `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`,
			want:       `truncated at max_output_tokens`,
		},
		{
			name:       "refusal",
			statusCode: http.StatusOK,
			body:       `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"cannot review"}]}]}`,
			want:       "refused review",
		},
		{
			name:       "no output text",
			statusCode: http.StatusOK,
			body:       `{"status":"completed","output":[{"type":"reasoning","content":[]}]}`,
			want:       "no output text",
		},
		{
			name:       "malformed response",
			statusCode: http.StatusOK,
			body:       `{`,
			want:       "unexpected end of JSON input",
		},
		{
			name:       "API error",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":{"message":"denied"}}`,
			want:       "openai API status 401",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			agent := &OpenAIAgent{
				APIKey:  "test-key",
				Model:   "gpt-4o-mini",
				BaseURL: server.URL,
				HTTP:    server.Client(),
			}
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

func TestACRPromptIsRecallOrientedAndLeavesTheVerdictToRadar(t *testing.T) {
	for _, want := range []string{"Report EVERY distinct P0 and P1", "at most the 5 most important P2 and P3", "tests_suggested", "where_to_look", "not an accept or reject from you"} {
		if !strings.Contains(acrSystemPrompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	for _, gatekeeper := range []string{"Auto-accept", `"accept"`, "auto-land"} {
		if strings.Contains(acrSystemPrompt, gatekeeper) {
			t.Fatalf("the shared prompt must not ask the model for a verdict (%q)", gatekeeper)
		}
	}
}

func TestParseACRVerdictKeepsP2BlockingByDefault(t *testing.T) {
	res, err := parseACRVerdict(`{"summary":"docs","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[{"severity":"P2","title":"t","file":"a.md","line":1,"end_line":1,"impact":"s","recommendation":"","rule_id":"","confidence":8}],"questions":[],"tests_suggested":[],"signals":{"safe":["doc-comment-update"],"risk":[]},"coverage":{"files_reviewed":["a.md"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accept || !res.ModelAccept {
		t.Fatalf("P2 must still block Accept while ModelAccept keeps the claim: %+v", res)
	}
}

func TestOpenAIAgentReportsUsageTruncationAndRequestOptions(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":900,"output_tokens":32000,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}},"output":[]}`))
	}))
	defer server.Close()
	agent := &OpenAIAgent{APIKey: "k", Model: "openai.gpt-5.6-terra", BaseURL: server.URL, HTTP: server.Client(),
		ReasoningEffort: "high", MaxOutputTokens: 32000, ExplicitCache: true, Provider: "bedrock-openai"}
	res := agent.Review(Diff{Changes: []Change{{File: "a", Content: "+x"}}})
	if !res.Truncated || res.Usage == nil || res.Usage.OutputTokens != 32000 || res.ElapsedMS == nil {
		t.Fatalf("got %+v usage=%+v", res, res.Usage)
	}
	if got["reasoning"].(map[string]any)["effort"] != "high" || got["max_output_tokens"].(float64) != 32000 ||
		got["prompt_cache_options"].(map[string]any)["mode"] != "explicit" {
		t.Fatalf("request = %v", got)
	}
	if agent.Describe() != "bedrock-openai/openai.gpt-5.6-terra" {
		t.Fatalf("describe = %q", agent.Describe())
	}
	plain := &OpenAIAgent{Model: "gpt-x"}
	if plain.Describe() != "openai/gpt-x" {
		t.Fatalf("default provider: %q", plain.Describe())
	}
}

func TestOpenAIAgentRecordsUsageOnSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"completed","usage":{"input_tokens":1134,"output_tokens":191,"input_tokens_details":{"cached_tokens":7,"cache_write_tokens":3}},"output":[{"type":"message","content":[{"type":"output_text","text":"{\"summary\":\"s\",\"risk\":{\"level\":\"low\",\"reason\":\"test\"},\"where_to_look\":[],\"findings\":[],\"questions\":[],\"tests_suggested\":[],\"signals\":{\"safe\":[\"doc-comment-update\"],\"risk\":[]},\"coverage\":{\"files_reviewed\":[\"a\"],\"repo_access\":false,\"limits\":\"\"},\"needs_human\":{\"required\":false,\"reason\":\"\"},\"confidence\":9}"}]}]}`))
	}))
	defer server.Close()
	res := (&OpenAIAgent{APIKey: "k", Model: "m", BaseURL: server.URL, HTTP: server.Client()}).Review(Diff{Changes: []Change{{File: "a"}}})
	if res.Usage == nil || res.Usage.InputTokens != 1134 || res.Usage.OutputTokens != 191 || res.Usage.CacheReadTokens != 7 || res.Usage.CacheWriteTokens != 3 || res.Usage.Requests != 1 {
		t.Fatalf("usage = %+v (summary %q)", res.Usage, res.Summary)
	}
}

func TestOpenAIAgentKeepsUsageOnRefusalAndEmptyOutput(t *testing.T) {
	for _, tt := range []struct{ name, output, want string }{
		{"refusal", `[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]`, "refused"},
		{"empty", `[]`, "no output text"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"completed","usage":{"input_tokens":1200,"output_tokens":40,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}},"output":` + tt.output + `}`))
			}))
			defer server.Close()
			res := (&OpenAIAgent{APIKey: "k", Model: "m", BaseURL: server.URL, HTTP: server.Client()}).Review(Diff{})
			if res.Accept || !strings.Contains(res.Summary, tt.want) || res.Usage == nil || res.Usage.InputTokens != 1200 || res.Usage.OutputTokens != 40 {
				t.Fatalf("summary %q usage %+v", res.Summary, res.Usage)
			}
		})
	}
}

// needs_human must mean a concrete reason a person must act that findings
// and Radar's own rules do not carry (both reviewers once set it on
// every PR when it meant "a person should look").
func TestNeedsHumanPromptIsNarrow(t *testing.T) {
	for _, want := range []string{
		"needs_human: false (reason \"\") for almost every change",
		"Radar already sends a change to a person when it has a P0 or P1 finding",
		"irreversible or destructive operation",
		"compliance, contractual or regulatory weight",
		"weakens or replaces a security control",
		"Do not set it for: a change being large, complex",
		"deserves a careful read",
		"that is a finding, at the right severity",
		"say so in coverage.limits",
	} {
		if !strings.Contains(acrSystemPrompt, want) {
			t.Fatalf("needs_human guidance missing %q", want)
		}
	}
	if strings.Contains(acrSystemPrompt, "a change you could not judge") {
		t.Fatal("not being able to judge a change is a coverage limit, not a reason for a human")
	}
}

func TestAssumptionsPromptSeparatesThemFromQuestions(t *testing.T) {
	for _, want := range []string{
		"- assumptions: at most 5 things you took as true",
		"load-bearing first",
		`"not visible in the diff"`,
		"Questions ask the author for something; assumptions record what you believed",
		`"assumptions": [{"assumption": string, "basis": string, "if_wrong": string, "load_bearing": bool}]`,
	} {
		if !strings.Contains(acrSystemPrompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
