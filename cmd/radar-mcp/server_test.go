package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeGitHub knows the repositories in repos, which wildcard entries look
// up.
type fakeGitHub struct {
	repos map[string]repositoryInfo
}

func (f fakeGitHub) repository(_ context.Context, repo string) (repositoryInfo, error) {
	info, ok := f.repos[strings.ToLower(repo)]
	if !ok {
		return repositoryInfo{}, fmt.Errorf("%w: %s", errRepositoryNotFound, repo)
	}
	return info, nil
}

func (fakeGitHub) reviewToken(context.Context, string) (string, error) { return "ghs_review", nil }

func (fakeGitHub) resolveCommit(_ context.Context, repo, sha string) (string, error) {
	if strings.HasPrefix(testSHA, sha) {
		return testSHA, nil
	}
	return "", fmt.Errorf("%w: %s is not a commit pushed to %s", errCommitNotFound, sha, repo)
}

type fakeRunner struct {
	mu       sync.Mutex
	requests []runRequest
	result   func(runRequest) (json.RawMessage, error)
	release  chan struct{}
}

func (f *fakeRunner) run(ctx context.Context, req runRequest) (json.RawMessage, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.result != nil {
		return f.result(req)
	}
	return json.RawMessage(fmt.Sprintf(`{"repository":%q,"head_sha":%q,"markdown":"## Radar\n\nLooks good.","review":{"decision":"would-approve"}}`, req.Repo, req.SHA)), nil
}

func (f *fakeRunner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newTestServer(t *testing.T, r reviewRunner) (*server, *memStore) {
	t.Helper()
	c, err := loadServerConfig(writeConfig(t, exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	c.Limits.ReviewWait.Duration = 2 * time.Second
	st := newMemStore()
	s := &server{
		config: c, version: "v0.13.0", packsDigest: "packs", pod: "radar-mcp-0", brokerURL: "http://127.0.0.1:8081/credentials",
		proxyToken: testProxyToken,
		github:     fakeGitHub{}, store: st, runner: r, broker: testBroker(), admit: newAdmission(c.Limits),
		metrics: newMetrics(prometheus.NewRegistry()), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: time.Now, reviews: context.Background(), done: map[reviewKey]chan struct{}{},
	}
	s.registerTools()
	return s, st
}

const testProxyToken = "proxy-token-0123456789abcdef0123456789"

// identityTransport stands in for the auth proxy: its secret and the
// identity it verified.
type identityTransport struct{ email, proxyToken string }

func (t identityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.proxyToken != "" {
		r.Header.Set(proxyTokenHeader, t.proxyToken)
	}
	if t.email != "" {
		r.Header.Set(actorEmailHeader, t.email)
		r.Header.Set(actorSubjectHeader, "sub-"+t.email)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, s *server, email string) (*mcp.ClientSession, error) {
	return connectVia(t, s, identityTransport{email: email, proxyToken: testProxyToken})
}

func connectVia(t *testing.T, s *server, transport http.RoundTripper) (*mcp.ClientSession, error) {
	t.Helper()
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL, HTTPClient: &http.Client{Transport: transport},
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = session.Close() })
	}
	return session, err
}

func callTool(t *testing.T, session *mcp.ClientSession, name, repo, sha string) (*mcp.CallToolResult, reviewOutput) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"repo": repo, "sha": sha}})
	if err != nil {
		t.Fatal(err)
	}
	var out reviewOutput
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out)
	}
	return res, out
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestReviewRunsOnceAndIsStored(t *testing.T) {
	runner := &fakeRunner{}
	s, st := newTestServer(t, runner)
	session, err := connect(t, s, "dev@example.com")
	if err != nil {
		t.Fatal(err)
	}

	res, out := callTool(t, session, "radar_review", "acme/backend", "0123456")
	if res.IsError || out.Status != statusCompleted {
		t.Fatalf("radar_review: %s %+v", text(res), out)
	}
	if out.SHA != testSHA || out.Repository != "Acme/backend" || out.RadarVersion != "v0.13.0" || out.RequestedBy != "dev@example.com" {
		t.Fatalf("output = %+v", out)
	}
	if !strings.Contains(text(res), "Looks good.") {
		t.Fatalf("text = %q, want the rendered review", text(res))
	}

	req := runner.requests[0]
	if req.GitHubToken != "ghs_review" || req.CredentialsURI != s.brokerURL || req.CredentialsToken == "" {
		t.Fatalf("run request credentials = %+v", req)
	}
	if req.Agent != "bedrock=us.anthropic.claude-opus-5-5,codex=openai.gpt-6.1-sol" || req.Env["RADAR_CODEX_REGION"] != "us-east-1" {
		t.Fatalf("run request settings = %+v", req)
	}
	// The broker revoked the review's credentials token when it ended.
	if len(s.broker.tokens) != 0 {
		t.Fatalf("broker still holds %d tokens", len(s.broker.tokens))
	}

	_, again := callTool(t, session, "radar_review", "Acme/backend", testSHA)
	_, status := callTool(t, session, "radar_status", "Acme/backend", testSHA)
	if again.Status != statusCompleted || status.Status != statusCompleted || runner.calls() != 1 {
		t.Fatalf("again=%s status=%s runs=%d; want the stored review, run once", again.Status, status.Status, runner.calls())
	}
	if len(st.results) != 1 || len(st.markers) != 0 {
		t.Fatalf("store: %d results, %d markers", len(st.results), len(st.markers))
	}
}

func TestStatusStartsNothing(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := newTestServer(t, runner)
	session, err := connect(t, s, "dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, out := callTool(t, session, "radar_status", "Acme/backend", testSHA)
	if out.Status != statusNotFound || runner.calls() != 0 {
		t.Fatalf("status = %+v, runs = %d", out, runner.calls())
	}
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	s, _ := newTestServer(t, &fakeRunner{})
	if _, err := connect(t, s, ""); err == nil {
		t.Fatal("connected without the auth proxy's identity headers")
	}
	// A process in the pod that forges an identity without the proxy's secret.
	if _, err := connectVia(t, s, identityTransport{email: "forged@example.com"}); err == nil {
		t.Fatal("connected with a forged identity and no proxy token")
	}
	if _, err := connectVia(t, s, identityTransport{email: "forged@example.com", proxyToken: "guess"}); err == nil {
		t.Fatal("connected with a wrong proxy token")
	}
}

func TestACallerSuppliedProxyTokenIsRefused(t *testing.T) {
	h := requireIdentity(testProxyToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Add(proxyTokenHeader, "caller")
	req.Header.Add(proxyTokenHeader, testProxyToken)
	req.Header.Set(actorEmailHeader, "a@example.com")
	req.Header.Set(actorSubjectHeader, "s")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 for two token values", rec.Code)
	}
}

func TestRepositoriesOffTheAllowlistAreRefused(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := newTestServer(t, runner)
	session, err := connect(t, s, "dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	res, _ := callTool(t, session, "radar_review", "Acme/secret-repo", testSHA)
	if !res.IsError || !strings.Contains(text(res), "does not review") || runner.calls() != 0 {
		t.Fatalf("result %q (error %v), runs %d", text(res), res.IsError, runner.calls())
	}
	res, _ = callTool(t, session, "radar_review", "Acme/backend", "fedcba9")
	if !res.IsError || !strings.Contains(text(res), "not a commit pushed") {
		t.Fatalf("unpushed commit: %q", text(res))
	}
}

// A review another replica is running is joined, not started again.
func TestAReviewRunningElsewhereIsJoined(t *testing.T) {
	runner := &fakeRunner{}
	s, st := newTestServer(t, runner)
	s.config.Limits.ReviewWait.Duration = 50 * time.Millisecond
	settings, k, err := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.claim(context.Background(), k, pendingRecord{State: stateRunning, RequestedBy: "other@example.com", Pod: "radar-mcp-1", StartedAt: time.Now()}, "")
	out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
	if err != nil || out.Status != statusRunning || runner.calls() != 0 {
		t.Fatalf("out=%+v err=%v runs=%d", out, err, runner.calls())
	}
}

func TestStaleAndFailedReviewsAreRetried(t *testing.T) {
	for name, marker := range map[string]pendingRecord{
		"stale":  {State: stateRunning, StartedAt: time.Now().Add(-2 * time.Hour)},
		"failed": {State: stateFailed, StartedAt: time.Now(), Error: "The review failed: boom."},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			s, st := newTestServer(t, runner)
			settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
			_, _ = st.claim(context.Background(), k, marker, "")
			out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
			if err != nil || out.Status != statusCompleted || runner.calls() != 1 {
				t.Fatalf("out=%+v err=%v runs=%d", out, err, runner.calls())
			}
		})
	}
}

func TestAResultForAnotherCommitIsNotStored(t *testing.T) {
	runner := &fakeRunner{result: func(req runRequest) (json.RawMessage, error) {
		return json.RawMessage(`{"repository":"Acme/backend","head_sha":"ffffffffffffffffffffffffffffffffffffffff"}`), nil
	}}
	s, st := newTestServer(t, runner)
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
	if err != nil || out.Status != statusFailed || !strings.Contains(out.Message, "not Acme/backend@"+testSHA) {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(st.results) != 0 {
		t.Fatal("stored a result under the wrong commit")
	}
}

// A commit Radar cannot review is a final answer: asking again reports it
// without running another review or charging for one.
func TestRejectedReviewsAreFinal(t *testing.T) {
	runner := &fakeRunner{result: func(runRequest) (json.RawMessage, error) {
		return nil, errRejected{"radar-gh review-commit: " + testSHA + " has no changes to review against main"}
	}}
	s, _ := newTestServer(t, runner)
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	who := actor{Email: "dev@example.com", Subject: "s"}
	out, _ := s.review(context.Background(), who, settings, k)
	if out.Status != statusRejected || !strings.Contains(out.Message, "cannot review this commit") || !strings.Contains(out.Message, "no changes") ||
		strings.Contains(out.Message, "try again") {
		t.Fatalf("out = %+v", out)
	}
	tokens := s.admit.buckets[who.Email].tokens
	again, err := s.review(context.Background(), who, settings, k)
	if err != nil || again.Status != statusRejected || runner.calls() != 1 || s.admit.buckets[who.Email].tokens != tokens {
		t.Fatalf("again=%+v err=%v runs=%d tokens %v -> %v; want the rejection reported, not rerun or charged",
			again, err, runner.calls(), tokens, s.admit.buckets[who.Email].tokens)
	}
}

// A failure, unlike a rejection, is retried.
func TestFailedReviewsAreRetried(t *testing.T) {
	calls := 0
	runner := &fakeRunner{}
	runner.result = func(req runRequest) (json.RawMessage, error) {
		if calls++; calls == 1 {
			return nil, errors.New("radar-gh: exit status 1: reading the default branch: GitHub 502")
		}
		return json.RawMessage(fmt.Sprintf(`{"repository":%q,"head_sha":%q}`, req.Repo, req.SHA)), nil
	}
	s, _ := newTestServer(t, runner)
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	who := actor{Email: "dev@example.com", Subject: "s"}
	if out, _ := s.review(context.Background(), who, settings, k); out.Status != statusFailed || !strings.Contains(out.Message, "try again") {
		t.Fatalf("first: %+v", out)
	}
	if out, _ := s.review(context.Background(), who, settings, k); out.Status != statusCompleted {
		t.Fatalf("retry: %+v", out)
	}
}

// reviewJSON is radar-gh review-commit -json's output for req with review as
// its radar.PullRequestReview.
func reviewJSON(req runRequest, review string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"repository":%q,"head_sha":%q,"markdown":"## Radar","review":%s}`, req.Repo, req.SHA, review))
}

const (
	// bedrockFailed is a lone reviewer that failed safe: Agent is its verdict
	// and there are no Reviews.
	bedrockFailed = `{"reviewer":"bedrock/us.anthropic.claude-opus-5-5","agent":{"accept":false,"confidence":0,
		"summary":"ACR Bedrock error, failing safe: ThrottlingException: Too many tokens","failed":true}}`
	// codexFailed is two reviewers, the second of which failed safe; Agent is
	// their merge, failed because one part is.
	codexFailed = `{"reviewer":"bedrock/us.anthropic.claude-opus-5-5+codex/openai.gpt-6.1-sol",
		"agent":{"summary":"bedrock/us.anthropic.claude-opus-5-5: Looks good.; codex/openai.gpt-6.1-sol: ACR codex error, failing safe: signal: killed","failed":true},
		"reviews":[
			{"reviewer":"bedrock/us.anthropic.claude-opus-5-5","passed":true,"agent":{"accept":true,"confidence":9,"summary":"Looks good."}},
			{"reviewer":"codex/openai.gpt-6.1-sol","passed":false,"agent":{"summary":"ACR codex error, failing safe: signal: killed","failed":true}}]}`
	// bothReviewed is two reviewers that both reviewed, one finding a bug: a
	// verdict against the change is a review, not a failure.
	bothReviewed = `{"reviewer":"bedrock/us.anthropic.claude-opus-5-5+codex/openai.gpt-6.1-sol",
		"agent":{"summary":"bedrock/us.anthropic.claude-opus-5-5: Looks good.; codex/openai.gpt-6.1-sol: A P1 bug."},
		"reviews":[
			{"reviewer":"bedrock/us.anthropic.claude-opus-5-5","passed":true,"agent":{"accept":true,"confidence":9,"summary":"Looks good."}},
			{"reviewer":"codex/openai.gpt-6.1-sol","passed":false,"agent":{"accept":false,"confidence":8,"summary":"A P1 bug."}}]}`
)

// A reviewer that failed safe (a Bedrock throttle, a timeout, an answer it
// could not parse) leaves radar-gh exiting 0 with a review that is not one.
// It is recorded as a failure, naming the reviewer and why, and not stored,
// where it would be served to everyone for the bucket's retention.
func TestAReviewWithAFailedReviewerIsNotStored(t *testing.T) {
	for name, tc := range map[string]struct{ review, failed, why, healthy string }{
		"the only reviewer":    {review: bedrockFailed, failed: "bedrock/us.anthropic.claude-opus-5-5", why: "ThrottlingException: Too many tokens"},
		"one of two reviewers": {review: codexFailed, failed: "codex/openai.gpt-6.1-sol", why: "signal: killed", healthy: "bedrock/"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{result: func(req runRequest) (json.RawMessage, error) { return reviewJSON(req, tc.review), nil }}
			s, st := newTestServer(t, runner)
			settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
			out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
			if err != nil || out.Status != statusFailed || !strings.Contains(out.Message, tc.failed) || !strings.Contains(out.Message, tc.why) ||
				!strings.Contains(out.Message, "try again") || (tc.healthy != "" && strings.Contains(out.Message, tc.healthy)) {
				t.Fatalf("out=%+v err=%v; want failed, naming %s and why", out, err, tc.failed)
			}
			if len(st.results) != 0 {
				t.Fatal("stored a review in which a reviewer failed")
			}
			if status, _ := s.status(context.Background(), settings, k); status.Status != statusFailed || status.Message != out.Message {
				t.Fatalf("radar_status = %+v, want the same failure", status)
			}
		})
	}
}

// A failed reviewer's summary carries its provider's error, which can run
// long; the message keeps the start of it, cut on a character boundary.
func TestAFailedReviewersSummaryIsBounded(t *testing.T) {
	v := reviewVerdicts{Reviewer: "bedrock/m", Agent: reviewerVerdict{Summary: "ACR Bedrock error, failing safe: " + strings.Repeat("é", 4096), Failed: true}}
	got := v.failed()
	if len(got) != 1 || !strings.HasPrefix(got[0], "bedrock/m: ACR Bedrock error") ||
		len(got[0]) > len("bedrock/m: ")+maxFailureSummary+len("…") || !utf8.ValidString(got[0]) {
		t.Fatalf("failed() = %q", got)
	}
}

// A review whose reviewer failed is run again on the next radar_review, like
// any failure, and the review that then finishes is stored.
func TestAReviewWithAFailedReviewerIsRetried(t *testing.T) {
	calls := 0
	runner := &fakeRunner{}
	runner.result = func(req runRequest) (json.RawMessage, error) {
		if calls++; calls == 1 {
			return reviewJSON(req, codexFailed), nil
		}
		return reviewJSON(req, bothReviewed), nil
	}
	s, st := newTestServer(t, runner)
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	who := actor{Email: "dev@example.com", Subject: "s"}
	if out, _ := s.review(context.Background(), who, settings, k); out.Status != statusFailed {
		t.Fatalf("first: %+v", out)
	}
	if out, _ := s.review(context.Background(), who, settings, k); out.Status != statusCompleted || runner.calls() != 2 {
		t.Fatalf("retry: %+v, runs %d; want a new review, completed", out, runner.calls())
	}
	if out, _ := s.review(context.Background(), who, settings, k); out.Status != statusCompleted || runner.calls() != 2 {
		t.Fatalf("after: %+v, runs %d; want the stored review", out, runner.calls())
	}
	if len(st.results) != 1 || len(st.markers) != 0 {
		t.Fatalf("store: %d results, %d markers", len(st.results), len(st.markers))
	}
}

// A review every reviewer finished is stored as radar-gh wrote it, whatever
// the reviewers concluded.
func TestAReviewEveryReviewerFinishedIsStored(t *testing.T) {
	var output json.RawMessage
	runner := &fakeRunner{result: func(req runRequest) (json.RawMessage, error) {
		output = reviewJSON(req, bothReviewed)
		return output, nil
	}}
	s, st := newTestServer(t, runner)
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
	if err != nil || out.Status != statusCompleted {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if rec, ok := st.results[k]; !ok || string(rec.Result) != string(output) {
		t.Fatalf("stored %q, want radar-gh's output %q", rec.Result, output)
	}
}

// radar_review answers "running" when the review outlasts its wait, and the
// review finishes in the background for radar_status.
func TestALongReviewAnswersRunningThenCompletes(t *testing.T) {
	runner := &fakeRunner{release: make(chan struct{})}
	s, _ := newTestServer(t, runner)
	s.config.Limits.ReviewWait.Duration = 50 * time.Millisecond
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	who := actor{Email: "dev@example.com", Subject: "s"}
	out, err := s.review(context.Background(), who, settings, k)
	if err != nil || out.Status != statusRunning {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	// The same person cannot start a second review meanwhile.
	_, otherKey, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/radar", SHA: testSHA})
	otherSettings, _ := s.config.settings("Acme/radar")
	if _, err := s.review(context.Background(), who, otherSettings, otherKey); err == nil || !strings.Contains(err.Error(), "limit of 1 running reviews") {
		t.Fatalf("second review: %v", err)
	}
	close(runner.release)
	s.wg.Wait()
	out, err = s.status(context.Background(), settings, k)
	if err != nil || out.Status != statusCompleted {
		t.Fatalf("after: out=%+v err=%v", out, err)
	}
}

// A drain that runs out of time cancels running reviews, which record a
// failure rather than leave a running marker to go stale.
func TestDrainCancelsAndRecordsFailure(t *testing.T) {
	runner := &fakeRunner{release: make(chan struct{})}
	s, st := newTestServer(t, runner)
	reviews, cancel := context.WithCancel(context.Background())
	s.reviews = reviews
	s.config.Limits.ReviewWait.Duration = 10 * time.Millisecond
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	if _, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k); err != nil {
		t.Fatal(err)
	}
	s.drain(10*time.Millisecond, cancel)
	p, _, _ := st.pending(context.Background(), k)
	if p == nil || p.State != stateFailed {
		t.Fatalf("marker = %+v, want failed", p)
	}
}

// racingStore lets another replica act at a chosen moment.
type racingStore struct {
	*memStore
	afterPending func()
	beforeClaim  func()
}

func (r *racingStore) pending(ctx context.Context, k reviewKey) (*pendingRecord, string, error) {
	p, v, err := r.memStore.pending(ctx, k)
	if r.afterPending != nil {
		r.afterPending()
		r.afterPending = nil
	}
	return p, v, err
}

func (r *racingStore) claim(ctx context.Context, k reviewKey, p pendingRecord, version string) (string, error) {
	if r.beforeClaim != nil {
		r.beforeClaim()
		r.beforeClaim = nil
	}
	return r.memStore.claim(ctx, k, p, version)
}

// Another replica claims between this one's read and its claim: this one
// joins that review, runs nothing, and does not charge the person.
func TestLosingTheClaimJoinsAndRefunds(t *testing.T) {
	runner := &fakeRunner{}
	s, mem := newTestServer(t, runner)
	s.config.Limits.ReviewWait.Duration = 20 * time.Millisecond
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	s.store = &racingStore{memStore: mem, beforeClaim: func() {
		_, _ = mem.claim(context.Background(), k, pendingRecord{State: stateRunning, Pod: "radar-mcp-1", StartedAt: time.Now()}, "")
	}}
	out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
	if err != nil || out.Status != statusRunning || runner.calls() != 0 {
		t.Fatalf("out=%+v err=%v runs=%d", out, err, runner.calls())
	}
	if b := s.admit.buckets["dev@example.com"]; b == nil || b.tokens != float64(s.config.Limits.ReviewsPerHour) {
		t.Fatalf("bucket = %+v, want the review refunded", b)
	}
}

// unansweredStore stands in for S3 when a claim's answer does not come back:
// the write commits (unless drop is set, for one that never arrived), then
// the claim reports err, as when the SDK's retry of a committed write gets
// 412 or the connection drops, or reports the request's own cancellation.
type unansweredStore struct {
	*memStore
	err         error
	drop        bool
	beforeClaim func()
}

func (u *unansweredStore) claim(ctx context.Context, k reviewKey, p pendingRecord, version string) (string, error) {
	if u.beforeClaim != nil {
		u.beforeClaim()
	}
	var claimed string
	if !u.drop {
		var err error
		if claimed, err = u.memStore.claim(ctx, k, p, version); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if u.err != nil {
		return "", u.err
	}
	return claimed, nil
}

// S3 commits the claim but its answer is lost. The marker is this replica's,
// so it runs the review rather than wait on its own marker until it goes
// stale.
func TestAClaimWhoseAnswerWasLostStillRuns(t *testing.T) {
	for name, answer := range map[string]error{
		"retry precondition failed": errPreconditionFailed,
		"connection reset":          errors.New("read tcp 10.0.0.1:443: connection reset by peer"),
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			s, mem := newTestServer(t, runner)
			settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
			s.store = &unansweredStore{memStore: mem, err: answer}
			out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
			s.wg.Wait()
			if err != nil || out.Status != statusCompleted || runner.calls() != 1 || len(mem.markers) != 0 {
				t.Fatalf("out=%+v err=%v runs=%d markers=%v", out, err, runner.calls(), mem.markers)
			}
		})
	}
}

// A client that disconnects while the claim is being written does not leave
// a running marker behind for a review nobody runs.
func TestACancelledRequestDoesNotOrphanItsClaim(t *testing.T) {
	runner := &fakeRunner{}
	s, mem := newTestServer(t, runner)
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.store = &unansweredStore{memStore: mem, beforeClaim: cancel}
	_, _ = s.review(ctx, actor{Email: "dev@example.com", Subject: "s"}, settings, k)
	s.wg.Wait()
	out, err := s.status(context.Background(), settings, k)
	switch {
	case err != nil:
		t.Fatal(err)
	case out.Status == statusCompleted && runner.calls() == 1:
	case out.Status == statusNotFound && runner.calls() == 0:
	default:
		t.Fatalf("status %+v after %d runs; want the review run, or nothing left behind", out, runner.calls())
	}
}

// A claim that never reached S3 is an error, and the person is not charged.
func TestAClaimThatFailedIsAnError(t *testing.T) {
	runner := &fakeRunner{}
	s, mem := newTestServer(t, runner)
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	s.store = &unansweredStore{memStore: mem, err: errors.New("dial tcp: i/o timeout"), drop: true}
	_, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
	if err == nil || !strings.Contains(err.Error(), "i/o timeout") || runner.calls() != 0 || len(mem.markers) != 0 {
		t.Fatalf("err=%v runs=%d markers=%v", err, runner.calls(), mem.markers)
	}
	if b := s.admit.buckets["dev@example.com"]; b == nil || b.tokens != float64(s.config.Limits.ReviewsPerHour) {
		t.Fatalf("bucket = %+v, want the review refunded", b)
	}
}

// boundedStore records the writes made without a deadline inside staleMargin.
type boundedStore struct {
	*memStore
	mu        sync.Mutex
	writes    []string
	unbounded []string
}

func (b *boundedStore) record(ctx context.Context, write string) {
	d, ok := ctx.Deadline()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes = append(b.writes, write)
	if !ok || time.Until(d) > staleMargin {
		b.unbounded = append(b.unbounded, write)
	}
}

func (b *boundedStore) claim(ctx context.Context, k reviewKey, p pendingRecord, version string) (string, error) {
	b.record(ctx, "claim")
	return b.memStore.claim(ctx, k, p, version)
}

func (b *boundedStore) fail(ctx context.Context, k reviewKey, p pendingRecord, version string) error {
	b.record(ctx, "fail")
	return b.memStore.fail(ctx, k, p, version)
}

func (b *boundedStore) finish(ctx context.Context, k reviewKey, r reviewRecord, version string) error {
	b.record(ctx, "finish")
	return b.memStore.finish(ctx, k, r, version)
}

// The writes that claim, finish and fail a review are bounded, so a hung S3
// call cannot hold a review, and the drain, forever.
func TestReviewStoreWritesAreBounded(t *testing.T) {
	calls := 0
	runner := &fakeRunner{}
	runner.result = func(req runRequest) (json.RawMessage, error) {
		if calls++; calls == 1 {
			return nil, errors.New("radar-gh: exit status 1")
		}
		return json.RawMessage(fmt.Sprintf(`{"repository":%q,"head_sha":%q}`, req.Repo, req.SHA)), nil
	}
	s, mem := newTestServer(t, runner)
	st := &boundedStore{memStore: mem}
	s.store = st
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	for _, want := range []string{statusFailed, statusCompleted} {
		out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
		s.wg.Wait()
		if err != nil || out.Status != want {
			t.Fatalf("out=%+v err=%v, want %s", out, err, want)
		}
	}
	if fmt.Sprint(st.writes) != "[claim fail claim finish]" || len(st.unbounded) != 0 {
		t.Fatalf("writes %v, unbounded %v", st.writes, st.unbounded)
	}
}

// Another replica finishes between this one's marker read and result read:
// the result is returned, not reviewed again.
func TestAReviewFinishedMeanwhileIsNotRerun(t *testing.T) {
	runner := &fakeRunner{}
	s, mem := newTestServer(t, runner)
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	v, _ := mem.claim(context.Background(), k, pendingRecord{State: stateRunning, Pod: "radar-mcp-1", StartedAt: time.Now()}, "")
	s.store = &racingStore{memStore: mem, afterPending: func() {
		_ = mem.finish(context.Background(), k, reviewRecord{Repository: k.Repo, SHA: k.SHA, Result: json.RawMessage(`{}`)}, v)
	}}
	out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
	if err != nil || out.Status != statusCompleted || runner.calls() != 0 {
		t.Fatalf("out=%+v err=%v runs=%d", out, err, runner.calls())
	}
}

// A replica whose review went stale and was taken over does not mark the
// new owner's review failed.
func TestAFailedReviewDoesNotOverwriteATakeover(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	k := reviewKey{Repo: "Acme/backend", SHA: testSHA, Version: "v", Config: "c"}
	mine, _ := st.claim(ctx, k, pendingRecord{State: stateRunning, Pod: "a"}, "")
	if _, err := st.claim(ctx, k, pendingRecord{State: stateRunning, Pod: "b"}, mine); err != nil {
		t.Fatal(err)
	}
	if err := st.fail(ctx, k, pendingRecord{State: stateFailed, Pod: "a"}, mine); !errors.Is(err, errPreconditionFailed) {
		t.Fatalf("fail = %v, want errPreconditionFailed", err)
	}
	if p, _, _ := st.pending(ctx, k); p.Pod != "b" || p.State != stateRunning {
		t.Fatalf("marker = %+v, want b's running review", p)
	}
}

func TestDrainingRefusesNewReviews(t *testing.T) {
	runner := &fakeRunner{}
	s, st := newTestServer(t, runner)
	s.draining = true
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	_, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k)
	if err == nil || !strings.Contains(err.Error(), "restarting") || runner.calls() != 0 || len(st.markers) != 0 {
		t.Fatalf("err=%v runs=%d markers=%d", err, runner.calls(), len(st.markers))
	}
}

// Each review's end-to-end duration is recorded by reviewer configuration
// and result, the data a review_wait limit will be chosen from.
func TestReviewDurationIsRecordedPerAgent(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := newTestServer(t, runner)
	settings, k, _ := s.resolve(context.Background(), commitInput{Repo: "Acme/backend", SHA: testSHA})
	if out, err := s.review(context.Background(), actor{Email: "dev@example.com", Subject: "s"}, settings, k); err != nil || out.Status != statusCompleted {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(s.metrics.reviewDuration)
	families, err := reg.Gather()
	if err != nil || len(families) != 1 || len(families[0].GetMetric()) != 1 {
		t.Fatalf("families %v, err %v", families, err)
	}
	m := families[0].GetMetric()[0]
	labels := map[string]string{}
	for _, l := range m.GetLabel() {
		labels[l.GetName()] = l.GetValue()
	}
	if labels["agent"] != settings.Agent || labels["result"] != "completed" || m.GetHistogram().GetSampleCount() != 1 {
		t.Fatalf("labels %v, samples %d; want agent %q, result completed, one sample", labels, m.GetHistogram().GetSampleCount(), settings.Agent)
	}
}

func newWildcardServer(t *testing.T, gh fakeGitHub) *server {
	t.Helper()
	s, _ := newTestServer(t, &fakeRunner{})
	c, err := loadServerConfig(writeConfig(t, wildcardConfig))
	if err != nil {
		t.Fatal(err)
	}
	s.config, s.github = c, gh
	return s
}

var wildcardRepos = map[string]repositoryInfo{
	"acme/radar":       {FullName: "Acme/radar"},
	"acme/old-thing":   {FullName: "Acme/old-thing", Archived: true},
	"acme/transferred": {FullName: "Elsewhere/transferred"},
}

// A wildcard admits a repository GitHub knows in that organization, under
// GitHub's name and the default policy, and refuses the rest as the
// caller's mistake.
func TestWildcardRepositoriesAreConfirmedWithGitHub(t *testing.T) {
	s := newWildcardServer(t, fakeGitHub{repos: wildcardRepos})
	settings, k, err := s.resolve(context.Background(), commitInput{Repo: "acme/RADAR", SHA: testSHA})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Repo != "Acme/radar" || k.Repo != "Acme/radar" || !settings.DefaultPolicy {
		t.Fatalf("settings %+v key %+v", settings, k)
	}
	for _, repo := range []string{"Acme/missing", "Acme/old-thing", "Acme/transferred"} {
		_, _, err := s.resolve(context.Background(), commitInput{Repo: repo, SHA: testSHA})
		if !errors.As(err, new(callerError)) {
			t.Errorf("%s: err = %v, want a caller error", repo, err)
		}
	}
}
