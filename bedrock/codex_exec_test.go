//go:build unix

package bedrock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/travisjeffery/radar"
)

func staticCreds() aws.CredentialsProvider {
	return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "token"))
}

func TestMantleProxySignsPinsCacheKeyAndCountsUsage(t *testing.T) {
	var gotAuth, gotPath, gotPlaceholder string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		gotPlaceholder = r.Header.Get("X-Amz-Security-Token")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		io.WriteString(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":40}}}}`+"\n\n")
	}))
	defer upstream.Close()

	proxy, err := startMantleProxy(upstream.URL, "radar-pr-review", &http.Client{
		Transport: &sigV4Transport{base: http.DefaultTransport, creds: staticCreds(), region: "us-east-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.close()

	for range 2 {
		req, _ := http.NewRequest(http.MethodPost, proxy.baseURL()+"/responses", strings.NewReader(`{"model":"openai.gpt-6.1-sol","prompt_cache_key":"session-uuid","stream":true}`))
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=radar-codex-placeholder/...")
		req.Header.Set("X-Amz-Security-Token", "placeholder")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "response.completed") {
			t.Fatalf("stream not passed through: %q", body)
		}
	}
	if gotPath != "/openai/v1/responses" || gotBody["prompt_cache_key"] != "radar-pr-review" || gotBody["model"] != "openai.gpt-6.1-sol" {
		t.Fatalf("path %q body %v", gotPath, gotBody)
	}
	if !strings.Contains(gotAuth, "Credential=AKIDEXAMPLE/") || !strings.Contains(gotAuth, "/us-east-1/bedrock-mantle/aws4_request") || gotPlaceholder != "token" {
		t.Fatalf("request not re-signed: auth %q token %q", gotAuth, gotPlaceholder)
	}
	want := radar.ReviewUsage{InputTokens: 200, OutputTokens: 14, CacheReadTokens: 120, CacheWriteTokens: 80, Requests: 2}
	got := proxy.usage()
	if got == nil || got.FirstRequest == nil || got.PrefixSHA256 == "" {
		t.Fatalf("usage = %+v, want the first request and the prefix hash", got)
	}
	if first := (radar.RequestUsage{InputTokens: 100, CacheReadTokens: 60, CacheWriteTokens: 40}); *got.FirstRequest != first {
		t.Fatalf("first request = %+v, want %+v", *got.FirstRequest, first)
	}
	got.FirstRequest, got.PrefixSHA256 = nil, ""
	if *got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}

	resp, err := http.Get("http://" + proxy.listener.Addr().String() + "/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a path outside the Responses API must not be forwarded, got %d", resp.StatusCode)
	}
}

// codexFirstRequest is the shape of Codex's first Responses request: its
// developer message, then the environment context and the task.
const codexFirstRequest = `{"model":"openai.gpt-6.1-sol","prompt_cache_key":"session-uuid","stream":true,"instructions":"codex","tools":[],"text":{"format":{"type":"json_schema"}},"input":[` +
	`{"type":"message","role":"developer","content":[{"type":"input_text","text":"radar"}]},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>"}]},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"diff"}]}]}`

func TestMantleProxyPrimesTheSharedPrefixOnce(t *testing.T) {
	var bodies []map[string]any
	var accepts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies, accepts = append(bodies, body), append(accepts, r.Header.Get("Accept"))
		if body["stream"] == false {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":"incomplete","usage":{"input_tokens":90,"output_tokens":16,"input_tokens_details":{"cached_tokens":88,"cache_write_tokens":0}}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":88,"cache_write_tokens":12}}}}`+"\n\n")
	}))
	defer upstream.Close()
	proxy, err := startMantleProxy(upstream.URL, "radar-pr-review", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.close()

	for range 2 {
		req, _ := http.NewRequest(http.MethodPost, proxy.baseURL()+"/responses", strings.NewReader(codexFirstRequest))
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if len(bodies) != 3 {
		t.Fatalf("upstream got %d requests, want the prime and two requests", len(bodies))
	}
	prime := bodies[0]
	input, _ := prime["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["role"] != "developer" {
		t.Fatalf("prime input = %v, want only the developer message", prime["input"])
	}
	if prime["stream"] != false || prime["max_output_tokens"] != float64(primeOutputTokens) || prime["prompt_cache_key"] != "radar-pr-review" || prime["instructions"] != "codex" || prime["text"] == nil || accepts[0] != "application/json" {
		t.Fatalf("prime = %v (Accept %q), want it unstreamed, capped, keyed and otherwise the request's", prime, accepts[0])
	}
	for _, body := range bodies[1:] {
		if input, _ := body["input"].([]any); len(input) != 3 || body["stream"] != true || body["max_output_tokens"] != nil {
			t.Fatalf("Codex's request changed beyond its cache key: %v", body)
		}
	}
	got := proxy.usage()
	if got == nil || got.FirstRequest == nil || *got.FirstRequest != (radar.RequestUsage{InputTokens: 100, CacheReadTokens: 88, CacheWriteTokens: 12}) {
		t.Fatalf("first request = %+v, want Codex's first request, not the prime", got)
	}
	got.FirstRequest, got.PrefixSHA256 = nil, ""
	if want := (radar.ReviewUsage{InputTokens: 290, OutputTokens: 30, CacheReadTokens: 264, CacheWriteTokens: 24, Requests: 3}); *got != want {
		t.Fatalf("usage = %+v, want the prime's included: %+v", *got, want)
	}
}

func TestMantleProxyGoesOnWhenPrimingFails(t *testing.T) {
	var requests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == false {
			http.Error(w, "throttled", http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":7}}}`+"\n\n")
	}))
	defer upstream.Close()
	proxy, err := startMantleProxy(upstream.URL, "radar-pr-review", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.close()
	resp, err := http.Post(proxy.baseURL()+"/responses", "application/json", strings.NewReader(codexFirstRequest))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "response.completed") || requests != 2 {
		t.Fatalf("status %d after %d upstream requests: %s", resp.StatusCode, requests, body)
	}
	if got := proxy.usage(); got == nil || got.Requests != 1 || got.InputTokens != 100 {
		t.Fatalf("usage = %+v, want only Codex's request", got)
	}
}

func TestPrimeRequestNeedsALeadingDeveloperMessageAndMore(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"message","role":"user","content":[]},{"type":"message","role":"developer","content":[]}]}`,
		`{"input":[{"type":"message","role":"developer","content":[]}]}`,
		`{"input":"just text"}`,
		`{}`,
		`not json`,
	} {
		if got := primeRequest([]byte(body)); got != nil {
			t.Fatalf("primeRequest(%s) = %s, want nil", body, got)
		}
	}
	if got := primeRequest([]byte(`{"input":[{"role":"system","content":"a"},{"role":"developer","content":"b"},{"role":"user","content":"c"}]}`)); !strings.Contains(string(got), `"input":[{"role":"system","content":"a"},{"role":"developer","content":"b"}]`) {
		t.Fatalf("primeRequest kept the wrong input: %s", got)
	}
}

func TestPinCacheKeyLeavesNonObjectsAlone(t *testing.T) {
	for _, body := range []string{"", "[1]", "not json", "null"} {
		if got := string(pinCacheKey([]byte(body), "k")); got != body {
			t.Fatalf("pinCacheKey(%q) = %q", body, got)
		}
	}
	if got := string(pinCacheKey([]byte(`{"a":1}`), "")); got != `{"a":1}` {
		t.Fatalf("an empty key must leave the body alone, got %q", got)
	}
}

// fakeCodex writes a script that records its arguments, environment and
// config, then runs body; it returns the agent and the record directory.
func fakeCodex(t *testing.T, body string) (*CodexExecAgent, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	if err := os.Mkdir(record, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$@" > ` + record + `/args
env > ` + record + `/env
cp "$CODEX_HOME/config.toml" ` + record + `/config.toml
cat > ` + record + `/prompt
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then out="$2"; fi
  shift
done
` + body
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "checkout")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	return &CodexExecAgent{
		Model: "openai.gpt-6.1-sol", Region: "us-east-1", Effort: "medium", Timeout: 10 * time.Second,
		Workdir: work, Binary: bin, CacheKey: "radar-pr-review", creds: staticCreds(), upstream: "http://127.0.0.1:1",
	}, record
}

func reviewDiff() radar.Diff {
	return radar.Diff{ID: "o/r#1", Changes: []radar.Change{{File: "a.go", Content: "@@ -1 +1 @@\n-a\n+b"}}}
}

func TestCodexExecAgentReview(t *testing.T) {
	t.Setenv("RADAR_TEST_SECRET", "do-not-leak")
	verdict := `{"summary":"fine","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[{"severity":"P3","title":"nit","file":"a.go","line":1,"end_line":1,"impact":"rename","recommendation":"","rule_id":"","confidence":8}],"questions":[],"tests_suggested":[],"signals":{"safe":["refactor-no-behavior-change"],"risk":[]},"coverage":{"files_reviewed":["a.go"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`
	agent, record := fakeCodex(t, `printf '%s' '`+verdict+`' > "$out"`)
	res := agent.Review(reviewDiff())
	if !res.ModelAccept || res.Failed || res.Confidence != 9 || len(res.Findings) != 1 || res.ElapsedMS == nil {
		t.Fatalf("result = %+v", res)
	}
	if agent.Describe() != "codex/openai.gpt-6.1-sol" {
		t.Fatalf("describe = %q", agent.Describe())
	}

	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(record, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	args := read("args")
	for _, want := range []string{"exec", "--sandbox\nread-only", "--output-schema", "--ephemeral", "--cd\n" + agent.Workdir} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q:\n%s", want, args)
		}
	}
	env := read("env")
	if strings.Contains(env, "do-not-leak") || strings.Contains(env, "AKIDEXAMPLE") {
		t.Fatalf("Codex must not inherit secrets:\n%s", env)
	}
	config := read("config.toml")
	for _, want := range []string{`model = "openai.gpt-6.1-sol"`, `model_provider = "amazon-bedrock"`, `web_search = "disabled"`, "[skills.bundled]\nenabled = false", `base_url = "http://127.0.0.1:`, "developer_instructions = ", "read-only access to a checkout"} {
		if !strings.Contains(config, want) {
			t.Fatalf("config missing %q:\n%s", want, config)
		}
	}
	if prompt := read("prompt"); !strings.Contains(prompt, "--- change 1: a.go") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestCodexExecAgentFailsSafe(t *testing.T) {
	tests := map[string]struct {
		body    string
		timeout time.Duration
		want    string
	}{
		"exit status": {body: "echo boom >&2; exit 3", want: "boom"},
		"no verdict":  {body: "exit 0", want: "no verdict"},
		"bad verdict": {body: `printf '{"summary":"","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":[],"risk":[]},"coverage":{"files_reviewed":[],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":0}' > "$out"`, want: "parsing codex verdict"},
		"timeout":     {body: "sleep 5", timeout: 200 * time.Millisecond, want: "exceeded"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			agent, _ := fakeCodex(t, tc.body)
			if tc.timeout > 0 {
				agent.Timeout = tc.timeout
			}
			res := agent.Review(reviewDiff())
			if res.Accept || res.ModelAccept || !res.Failed || !strings.Contains(res.Summary, "failing safe") || !strings.Contains(res.Summary, tc.want) {
				t.Fatalf("summary = %q", res.Summary)
			}
		})
	}
}

func TestNewCodexExecAgentConfiguration(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-2")
	t.Setenv("RADAR_CODEX_REGION", "us-east-1")
	t.Setenv("RADAR_CODEX_BIN", "/bin/sh")
	t.Setenv("RADAR_CODEX_EFFORT", "")
	t.Setenv("RADAR_CODEX_TIMEOUT", "")
	t.Setenv("RADAR_CODEX_CACHE_KEY", "")
	dir := t.TempDir()
	a, err := NewCodexExecAgent(context.Background(), "openai.gpt-6.1-sol", dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.Region != "us-east-1" || a.Effort != "medium" || a.Timeout != 900*time.Second || a.CacheKey != "radar-pr-review" || a.Workdir != dir {
		t.Fatalf("agent = %+v", a)
	}
	for _, tc := range []struct{ model, dir string }{
		{"gpt-6.1-sol", dir}, {"openai.", dir}, {"openai.gpt-6.1-sol", ""}, {"openai.gpt-6.1-sol", filepath.Join(dir, "missing")},
	} {
		if _, err := NewCodexExecAgent(context.Background(), tc.model, tc.dir); err == nil {
			t.Fatalf("model %q dir %q must be rejected", tc.model, tc.dir)
		}
	}
	t.Setenv("RADAR_CODEX_TIMEOUT", "soon")
	if _, err := NewCodexExecAgent(context.Background(), "openai.gpt-6.1-sol", dir); err == nil {
		t.Fatal("a bad timeout must be rejected")
	}
}

func TestCodexExecAgentClarify(t *testing.T) {
	answer := `{"adjudications":[{"finding_id":"abc123","outcome":"withdrawn","severity":"P1","basis":"verified","evidence":[{"file":"a.go","line":1,"end_line":1}],"steering":false,"reply":"The caller validates it."}]}`
	agent, record := fakeCodex(t, `printf '%s' '`+answer+`' > "$out"`)
	req := radar.ClarifyRequest{
		HeadSHA:        "deadbeef",
		Findings:       []radar.MergedFinding{{ReviewFinding: radar.ReviewFinding{ID: "abc123", Severity: "P1", Title: "Unchecked id", File: "a.go", Line: 1, Summary: "panics"}, Reviewers: []string{"codex/x"}}},
		Clarifications: []radar.Clarification{{CommentID: 7, Kind: "inline", Author: "dev", Body: "the caller checks this", FindingIDs: []string{"abc123"}}},
	}
	res := agent.Clarify(req)
	if res.Failed || len(res.Adjudications) != 1 || res.Adjudications[0].Outcome != radar.OutcomeWithdrawn || res.Adjudicator != "codex/openai.gpt-6.1-sol" || res.ElapsedMS == nil {
		t.Fatalf("result = %+v", res)
	}
	config, _ := os.ReadFile(filepath.Join(record, "config.toml"))
	if !strings.Contains(string(config), "re-checking findings") || strings.Contains(string(config), "Favour recall") {
		t.Fatalf("clarify must use the clarification prompt:\n%s", config)
	}
	prompt, _ := os.ReadFile(filepath.Join(record, "prompt"))
	if !strings.Contains(string(prompt), "<untrusted_clarification>") || !strings.Contains(string(prompt), "the caller checks this") {
		t.Fatalf("prompt = %q", prompt)
	}

	agent, _ = fakeCodex(t, "exit 0")
	if res := agent.Clarify(req); !res.Failed || res.Adjudications != nil || res.Error == "" {
		t.Fatalf("a failed re-check must fail safe: %+v", res)
	}
}

// The proxy counts Codex's tool calls once each, though every request
// resends the conversation, by program for shell commands, and never changes
// the request.
func TestMantleProxyCountsCodexToolCalls(t *testing.T) {
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer upstream.Close()
	proxy, err := startMantleProxy(upstream.URL, "", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.close()

	turn1 := `{"instructions":"review","tools":[{"type":"function","name":"shell"}],"input":[` +
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"rg -n foo src\"]}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"src/a.go:1:foo"}]}`
	turn2 := `{"instructions":"review","tools":[{"type":"function","name":"shell"}],"input":[` +
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"rg -n foo src\"]}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"src/a.go:1:foo"},` +
		`{"type":"function_call","call_id":"c2","name":"exec_command","arguments":"{\"cmd\":\"cd pkg && sed -n 1,20p a.go\"}"},` +
		`{"type":"function_call_output","call_id":"c2","output":"0123456789"},` +
		`{"type":"function_call","call_id":"c3","name":"apply_patch","arguments":"{}"}]}`
	for _, body := range []string{turn1, turn2} {
		resp, err := http.Post(proxy.baseURL()+"/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	want := map[string]radar.ToolUse{
		"shell:rg":    {Calls: 1, Bytes: len("src/a.go:1:foo")},
		"shell:sed":   {Calls: 1, Bytes: 10},
		"apply_patch": {Calls: 1},
	}
	got := proxy.tools()
	if len(got) != len(want) {
		t.Fatalf("tools = %+v, want %+v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("tools[%s] = %+v, want %+v (all %+v)", k, got[k], v, got)
		}
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], `"call_id":"c3"`) {
		t.Fatalf("requests must pass through unchanged: %q", bodies)
	}
}
