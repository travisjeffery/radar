package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLinkedIssueKey(t *testing.T) {
	for _, tt := range []struct{ title, branch, want string }{
		{"ENG-123: Fold the bot", "", "ENG-123"},
		{"Fix flake", "tj/eng-123-fold-the-codex", "ENG-123"},
		{"Bump grpc to 1.2-3", "dependabot/go/grpc", ""},
		{"OPS-4567 role", "x", "OPS-4567"},
	} {
		if got := linkedIssueKey(tt.title, tt.branch); got != tt.want {
			t.Fatalf("linkedIssueKey(%q, %q) = %q, want %q", tt.title, tt.branch, got, tt.want)
		}
	}
}

func TestFetchLinearIssue(t *testing.T) {
	var auth string
	var variables map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		variables = body.Variables
		_, _ = w.Write([]byte(`{"data":{"issue":{"identifier":"ENG-123","title":"Fold the bot","description":"One review run per PR."}}}`))
	}))
	defer server.Close()
	old := linearGraphQLURL
	linearGraphQLURL = server.URL
	defer func() { linearGraphQLURL = old }()

	t.Setenv("LINEAR_API_KEY", "")
	if _, err := fetchLinearIssue("ENG-123"); err == nil {
		t.Fatal("without a key the issue is skipped")
	}
	t.Setenv("LINEAR_API_KEY", "lin_api_x")
	issue, err := fetchLinearIssue("ENG-123")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Key != "ENG-123" || issue.Title != "Fold the bot" || issue.Summary != "One review run per PR." || auth != "lin_api_x" || variables["id"] != "ENG-123" {
		t.Fatalf("issue=%+v auth=%q vars=%v", issue, auth, variables)
	}
}

func TestCheckStatesKeepProducersApart(t *testing.T) {
	runs := []githubCheckRun{
		{ID: 9, Name: "build", Status: "completed", Conclusion: "success"},
		{ID: 3, Name: "build", Status: "completed", Conclusion: "failure"},
	}
	runs[0].App.Slug, runs[1].App.Slug = "github-actions", "buildkite"
	statuses := []githubStatus{{ID: 99, Context: "build", State: "pending"}}
	got := checkStates(runs, statuses)
	if len(got) != 3 {
		t.Fatalf("each producer keeps its own entry: %+v", got)
	}
	want := map[string]string{"build": "success", "buildkite/build": "failure", "build (status)": "pending"}
	for _, c := range got {
		if want[c.Name] != c.State {
			t.Fatalf("got %+v", got)
		}
	}
}

func TestThreadBodyIncludesReplies(t *testing.T) {
	var nodes []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(`[{"author":{"login":"github-actions"},"body":"**P1: x**\n\n<!-- radar-finding abc123 -->"},{"author":{"login":"dev"},"body":"/radar the caller checks this"}]`), &nodes); err != nil {
		t.Fatal(err)
	}
	got := threadBody(nodes)
	if got != "**P1: x**\n(reply from dev): /radar the caller checks this" {
		t.Fatalf("body = %q", got)
	}
}
