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
					{"type":"output_text","text":"{\"accept\":true,\"confidence\":10,\"risk_signals\":[],"},
					{"type":"output_text","text":"\"safe_signals\":[\"doc-comment-update\"],\"reviewed_files\":[\"README.md\"],\"findings\":[],\"summary\":\"documentation only\"}"}
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
	if len(properties) != 7 {
		t.Fatalf("schema has %d properties, want 7", len(properties))
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
			want:       `status "incomplete": max_output_tokens`,
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
