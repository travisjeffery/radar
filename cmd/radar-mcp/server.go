package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	actorEmailHeader   = "X-Radar-MCP-Email"
	actorSubjectHeader = "X-Radar-MCP-Subject"
	requestIDHeader    = "X-Request-ID"

	statusCompleted = "completed"
	statusRunning   = "running"
	statusFailed    = "failed"
	statusRejected  = "rejected"
	statusNotFound  = "not_found"

	// staleMargin is how long past ReviewTimeout a running marker is trusted.
	// A replica that died mid-review leaves its marker; after this the next
	// request starts the review again.
	staleMargin = 5 * time.Minute
	pollEvery   = 3 * time.Second

	// storeTimeout bounds each store write that must not be cancelled with
	// the request or review making it: the claim, and recording a review's
	// result or failure. It leaves room for the SDK's retries and a result of
	// maxResultBytes and is far inside staleMargin, so a hung S3 call fails
	// the write rather than hold a review slot, and the drain, forever.
	storeTimeout = 30 * time.Second
)

// reviewRunner runs one review in the reviewer container.
type reviewRunner interface {
	run(ctx context.Context, req runRequest) (json.RawMessage, error)
}

type server struct {
	config      *serverConfig
	version     string
	packsDigest string
	pod         string
	brokerURL   string
	proxyToken  string

	github  commitSource
	store   store
	runner  reviewRunner
	broker  *credentialBroker
	admit   *admission
	metrics *metrics
	logger  *slog.Logger
	now     func() time.Time

	// reviews is cancelled when a drain runs out of time.
	reviews  context.Context
	wg       sync.WaitGroup
	mu       sync.Mutex
	draining bool
	done     map[reviewKey]chan struct{}

	mcp *mcp.Server
}

type actor struct {
	Email, Subject, RequestID string
}

type commitInput struct {
	Repo string `json:"repo" jsonschema:"GitHub owner/repository, for example Acme/backend"`
	SHA  string `json:"sha" jsonschema:"A commit pushed to the repository: a full or abbreviated SHA (at least 7 hex characters)"`
}

// reviewOutput is both tools' structured result.
type reviewOutput struct {
	Status       string     `json:"status" jsonschema:"completed, running, failed (retry with radar_review), rejected (Radar cannot review this commit) or not_found"`
	Repository   string     `json:"repository"`
	SHA          string     `json:"sha" jsonschema:"The full commit SHA reviewed"`
	RadarVersion string     `json:"radar_version"`
	ConfigDigest string     `json:"config_digest" jsonschema:"Identifies the reviewers and settings; a different digest is a different review"`
	Message      string     `json:"message,omitempty"`
	RequestedBy  string     `json:"requested_by,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	// Result is radar-gh review-commit's JSON: the base it reviewed against,
	// the structured review and the rendered Markdown.
	Result any `json:"result,omitempty" jsonschema:"The structured Radar review, as CI produces it"`
}

const instructions = `Radar reviews a pushed commit as Radar's CI review would review a pull request from it into the repository's default branch: the same reviewers, policy (read from the default branch) and rendered review. It is report-only: it never comments on, approves or changes anything on GitHub. Push the commit first. radar_review starts a review, or returns the stored one, and waits up to a minute; reviews take several minutes, so when it answers "running", call radar_status with the same repo and sha until it completes.`

func (s *server) registerTools() {
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "radar", Version: s.version}, &mcp.ServerOptions{Instructions: instructions})
	repos := strings.Join(s.config.repositories(), ", ")
	notDestructive := false
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "radar_review",
		Description: "Run Radar's CI code review on a commit pushed to GitHub, before any pull request exists, or return the stored review of it. " +
			"Report-only. Repositories: " + repos + ".",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &notDestructive, IdempotentHint: true, OpenWorldHint: &notDestructive},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in commitInput) (*mcp.CallToolResult, reviewOutput, error) {
		return s.call(ctx, req, "radar_review", in, s.review)
	})
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "radar_status",
		Description: "Return the stored Radar review of a pushed commit, or whether one is running. Starts nothing.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in commitInput) (*mcp.CallToolResult, reviewOutput, error) {
		return s.call(ctx, req, "radar_status", in, func(ctx context.Context, _ actor, settings reviewSettings, k reviewKey) (reviewOutput, error) {
			return s.status(ctx, settings, k)
		})
	})
}

type toolFunc func(ctx context.Context, who actor, settings reviewSettings, k reviewKey) (reviewOutput, error)

// call authenticates, resolves the commit, runs the tool and records the
// outcome. Errors are returned to the client as tool errors.
func (s *server) call(ctx context.Context, req *mcp.CallToolRequest, tool string, in commitInput, fn toolFunc) (*mcp.CallToolResult, reviewOutput, error) {
	started := s.now()
	result := "success"
	var out reviewOutput
	who, err := actorFromRequest(req)
	if err == nil {
		var settings reviewSettings
		var k reviewKey
		settings, k, err = s.resolve(ctx, in)
		if err == nil {
			out, err = fn(ctx, who, settings, k)
		}
	}
	switch {
	case err == nil:
		result = out.Status
	case errors.As(err, new(callerError)):
		result = "rejected"
	case who.Email == "":
		result = "unauthorized"
	default:
		result = "error"
	}
	s.metrics.calls.WithLabelValues(tool, result).Inc()
	s.metrics.callDurations.WithLabelValues(tool).Observe(time.Since(started).Seconds())
	level := slog.LevelInfo
	if result == "error" {
		level = slog.LevelError
	}
	s.logger.Log(ctx, level, "radar MCP tool call", "tool", tool, "result", result, "repo", out.Repository, "sha", out.SHA,
		"duration", time.Since(started), "actor_email", who.Email, "actor_subject", who.Subject, "request_id", who.RequestID, "error", errString(err))
	if err != nil {
		return nil, reviewOutput{}, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: summary(out)}}}, out, nil
}

// callerError is the caller's mistake (a repository not on the allowlist, a
// commit that is not pushed, a limit reached), kept out of the error count
// alerts watch.
type callerError struct{ error }

func (s *server) resolve(ctx context.Context, in commitInput) (reviewSettings, reviewKey, error) {
	repo, sha := strings.TrimSpace(in.Repo), strings.ToLower(strings.TrimSpace(in.SHA))
	settings, ok := s.config.settings(repo)
	if !ok {
		return reviewSettings{}, reviewKey{}, callerError{fmt.Errorf("radar does not review %q; it reviews %s", repo, strings.Join(s.config.repositories(), ", "))}
	}
	if !shortSHA.MatchString(sha) {
		return reviewSettings{}, reviewKey{}, callerError{errors.New("sha must be a commit SHA of 7 to 40 hex characters")}
	}
	if settings.Wildcard {
		var err error
		if settings.Repo, err = s.wildcardRepository(ctx, settings.Repo); err != nil {
			return reviewSettings{}, reviewKey{}, err
		}
	}
	full, err := s.github.resolveCommit(ctx, settings.Repo, sha)
	if errors.Is(err, errCommitNotFound) || errors.Is(err, errRepositoryNotFound) {
		return reviewSettings{}, reviewKey{}, callerError{err}
	}
	if err != nil {
		return reviewSettings{}, reviewKey{}, err
	}
	return settings, reviewKey{Repo: settings.Repo, SHA: full, Version: s.version, Config: configDigest(settings, s.packsDigest)}, nil
}

// wildcardRepository admits a repository through its owner's "owner/*"
// entry, and returns GitHub's name for it. It fails closed: the repository
// must be one the App reads in that same organization (a transferred
// repository's old name redirects elsewhere) and not archived.
func (s *server) wildcardRepository(ctx context.Context, repo string) (string, error) {
	owner, _, _ := strings.Cut(repo, "/")
	info, err := s.github.repository(ctx, repo)
	if errors.Is(err, errRepositoryNotFound) {
		return "", callerError{err}
	}
	if err != nil {
		return "", err
	}
	gotOwner, _, _ := strings.Cut(info.FullName, "/")
	switch {
	case !strings.EqualFold(gotOwner, owner):
		return "", callerError{fmt.Errorf("%s is now %s, outside %s; radar does not review it", repo, info.FullName, owner)}
	case info.Archived:
		return "", callerError{fmt.Errorf("%s is archived; nothing new can be pushed to it", info.FullName)}
	}
	return info.FullName, nil
}

func (s *server) output(k reviewKey, status string) reviewOutput {
	return reviewOutput{Status: status, Repository: k.Repo, SHA: k.SHA, RadarVersion: k.Version, ConfigDigest: k.Config}
}

// status reports what the store holds for k.
func (s *server) status(ctx context.Context, _ reviewSettings, k reviewKey) (reviewOutput, error) {
	rec, err := s.store.result(ctx, k)
	if err != nil {
		return reviewOutput{}, err
	}
	if rec != nil {
		return completedOutput(k, rec), nil
	}
	p, _, err := s.store.pending(ctx, k)
	if err != nil {
		return reviewOutput{}, err
	}
	out := s.output(k, statusNotFound)
	switch {
	case p == nil:
		out.Message = "No review of this commit has been run with this Radar version and configuration. Call radar_review to start one."
	case p.State == stateFailed:
		out.Status, out.Message = statusFailed, p.Error+" Call radar_review to try again."
	case p.State == stateRejected:
		out.Status, out.Message = statusRejected, p.Error
	case s.stale(p):
		out.Message = "A review was started but never finished. Call radar_review to start it again."
	default:
		out.Status = statusRunning
		out.Message = "The review is running. Call radar_status again in a minute."
	}
	if p != nil {
		out.RequestedBy, out.StartedAt = p.RequestedBy, &p.StartedAt
	}
	return out, nil
}

func completedOutput(k reviewKey, rec *reviewRecord) reviewOutput {
	out := reviewOutput{
		Status: statusCompleted, Repository: k.Repo, SHA: k.SHA, RadarVersion: k.Version, ConfigDigest: k.Config,
		RequestedBy: rec.RequestedBy, StartedAt: &rec.StartedAt, FinishedAt: &rec.FinishedAt,
	}
	var result map[string]any
	if json.Unmarshal(rec.Result, &result) == nil {
		out.Result = result
	}
	return out
}

func (s *server) stale(p *pendingRecord) bool {
	return s.now().Sub(p.StartedAt) > s.config.Limits.ReviewTimeout.Duration+staleMargin
}

// review returns the stored review, or joins the running one, or starts one.
func (s *server) review(ctx context.Context, who actor, settings reviewSettings, k reviewKey) (reviewOutput, error) {
	// One read decides. The marker is read before the result, so a review
	// that finishes in between shows as its result rather than as an absent
	// marker this replica would claim and run again.
	p, version, err := s.store.pending(ctx, k)
	if err != nil {
		return reviewOutput{}, err
	}
	rec, err := s.store.result(ctx, k)
	if err != nil {
		return reviewOutput{}, err
	}
	switch {
	case rec != nil:
		return completedOutput(k, rec), nil
	case p != nil && p.State == stateRunning && !s.stale(p):
		return s.wait(ctx, settings, k)
	case p != nil && p.State == stateRejected:
		// Final: report it again rather than run, and charge for, a review
		// that cannot succeed.
		return s.status(ctx, settings, k)
	}

	release, admitResult := s.admit.admit(who.Email)
	s.metrics.admissions.WithLabelValues(admitResult).Inc()
	switch admitResult {
	case admitBusy:
		return reviewOutput{}, callerError{errors.New("radar is running as many reviews as it can; try again in a few minutes")}
	case admitInFlight:
		return reviewOutput{}, callerError{fmt.Errorf("you have reached your limit of %d running reviews; wait for one with radar_status", s.config.Limits.InFlightPerUser)}
	case admitRateLimit:
		return reviewOutput{}, callerError{fmt.Errorf("you have started %d reviews in the last hour, the limit; stored reviews are still available with radar_status", s.config.Limits.ReviewsPerHour)}
	}

	// Register with the drain before claiming, so a shutdown either refuses
	// this review or waits for it, never leaves its marker running.
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		release(true)
		return reviewOutput{}, callerError{errors.New("radar is restarting; try again in a minute")}
	}
	s.wg.Add(1)
	s.mu.Unlock()

	// Claim the marker at the version read above: absent, failed or stale.
	// Losing means another replica just started it; join that instead, and
	// the person is not charged for a review they did not start.
	marker := pendingRecord{State: stateRunning, RequestedBy: who.Email, Pod: s.pod, StartedAt: s.now().UTC()}
	claimed, err := s.claim(ctx, k, marker, version)
	if err != nil {
		s.wg.Done()
		release(true)
		if errors.Is(err, errPreconditionFailed) {
			return s.wait(ctx, settings, k)
		}
		return reviewOutput{}, err
	}

	done := make(chan struct{})
	s.mu.Lock()
	s.done[k] = done
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer release(false)
		defer func() {
			s.mu.Lock()
			delete(s.done, k)
			s.mu.Unlock()
			close(done)
		}()
		s.run(settings, k, marker, claimed)
	}()
	return s.wait(ctx, settings, k)
}

// claim writes marker over the marker at version and returns its new
// version, or errPreconditionFailed if another replica claimed the review
// first.
//
// The write is detached from the request: a client that disconnects must not
// cancel a PUT that S3 may already have committed. Nor does an error prove
// the write failed. If S3 commits the PUT but the response is lost, the SDK's
// retry finds the marker changed and gets 412; a timeout or a dropped
// connection leaves the outcome unknown. Taken at its word, either would
// leave this replica's own running marker, for a review nobody runs, blocking
// the commit until it went stale. So after any error the marker is read back,
// and if it is the one just written (the same pod, person and start, to the
// nanosecond), the claim succeeded at the version read.
func (s *server) claim(ctx context.Context, k reviewKey, marker pendingRecord, version string) (string, error) {
	writeCtx, cancel := detached(ctx)
	claimed, err := s.store.claim(writeCtx, k, marker, version)
	cancel()
	if err == nil {
		return claimed, nil
	}
	readCtx, cancel := detached(ctx)
	defer cancel()
	p, current, rerr := s.store.pending(readCtx, k)
	mine := rerr == nil && p != nil && p.State == marker.State && p.Pod == marker.Pod &&
		p.RequestedBy == marker.RequestedBy && p.StartedAt.Equal(marker.StartedAt)
	if !mine {
		return "", err
	}
	s.logger.Warn("claimed a review whose claim reported an error", "key", k.String(), "error", err)
	return current, nil
}

// detached is the context for a store call that must complete whatever
// happens to the request or review making it, yet cannot hang: it keeps
// ctx's values but not its cancellation or deadline, and has its own
// storeTimeout.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
}

// wait waits up to ReviewWait for k's review, then reports its status.
func (s *server) wait(ctx context.Context, settings reviewSettings, k reviewKey) (reviewOutput, error) {
	deadline := time.NewTimer(s.config.Limits.ReviewWait.Duration)
	defer deadline.Stop()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	s.mu.Lock()
	done := s.done[k]
	s.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return reviewOutput{}, ctx.Err()
		case <-deadline.C:
			return s.status(ctx, settings, k)
		case <-done:
			return s.status(ctx, settings, k)
		case <-tick.C:
			// Another replica may be running it; its result lands in the store.
			out, err := s.status(ctx, settings, k)
			if err != nil || out.Status != statusRunning {
				return out, err
			}
		}
	}
}

// run runs a claimed review to a stored result or a failed marker. It is
// detached from the request that started it: closing the client does not
// stop a review someone else may be waiting for.
func (s *server) run(settings reviewSettings, k reviewKey, marker pendingRecord, version string) {
	ctx, cancel := context.WithTimeout(s.reviews, s.config.Limits.ReviewTimeout.Duration)
	defer cancel()
	s.metrics.inFlight.Inc()
	defer s.metrics.inFlight.Dec()
	started := s.now()

	output, err := s.execute(ctx, settings, k)
	result := "completed"
	if err == nil {
		storeCtx, cancel := detached(ctx)
		err = s.store.finish(storeCtx, k, reviewRecord{
			Repository: k.Repo, SHA: k.SHA, RadarVersion: k.Version, ConfigDigest: k.Config,
			RequestedBy: marker.RequestedBy, StartedAt: marker.StartedAt, FinishedAt: s.now().UTC(), Result: output,
		}, version)
		cancel()
	}
	if err != nil {
		state := stateFailed
		result = "failed"
		msg := "The review failed: " + err.Error()
		if errors.As(err, new(errRejected)) {
			result, state = "rejected", stateRejected
			msg = "Radar cannot review this commit: " + err.Error()
		}
		marker.State, marker.Error = state, msg
		// A replica that took the review over (ours went stale) owns the
		// marker now; leave it.
		storeCtx, cancel := detached(ctx)
		if ferr := s.store.fail(storeCtx, k, marker, version); ferr != nil && !errors.Is(ferr, errPreconditionFailed) {
			s.logger.Error("recording a failed review", "key", k.String(), "error", ferr)
		}
		cancel()
	}
	s.metrics.reviews.WithLabelValues(result).Inc()
	s.metrics.reviewDuration.WithLabelValues(settings.Agent, result).Observe(time.Since(started).Seconds())
	s.logger.Info("radar review", "key", k.String(), "agent", settings.Agent, "result", result, "duration", time.Since(started),
		"actor_email", marker.RequestedBy, "error", errString(err))
}

// execute gives the reviewer container this review's credentials and runs it.
func (s *server) execute(ctx context.Context, settings reviewSettings, k reviewKey) (json.RawMessage, error) {
	token, err := s.github.reviewToken(ctx, k.Repo)
	if err != nil {
		return nil, err
	}
	credsToken, revoke := s.broker.issue()
	defer revoke()
	output, err := s.runner.run(ctx, runRequest{
		Repo: k.Repo, SHA: k.SHA, Agent: settings.Agent, Policy: settings.Policy, DefaultPolicy: settings.DefaultPolicy, Env: settings.Env,
		GitHubToken: token, CredentialsURI: s.brokerURL, CredentialsToken: credsToken,
	})
	if err != nil {
		return nil, err
	}
	// The result must be the commit asked for, so the stored review cannot be
	// filed under another commit's key.
	var bound struct {
		Repository string         `json:"repository"`
		HeadSHA    string         `json:"head_sha"`
		Review     reviewVerdicts `json:"review"`
	}
	if err := json.Unmarshal(output, &bound); err != nil {
		return nil, fmt.Errorf("reading radar-gh's result: %w", err)
	}
	if !strings.EqualFold(bound.Repository, k.Repo) || bound.HeadSHA != k.SHA {
		return nil, fmt.Errorf("radar-gh reviewed %s@%s, not %s@%s", bound.Repository, bound.HeadSHA, k.Repo, k.SHA)
	}
	// A reviewer that failed safe (a provider error, a timeout, an answer it
	// could not parse) still lets radar-gh exit 0, with a review that needs a
	// human for no reason in the change. Stored, one Bedrock throttle would be
	// everyone's review of this commit until the bucket expires it, with no
	// way to run it again. As CI records no failed review, it is a failure,
	// which the next radar_review retries.
	if failed := bound.Review.failed(); len(failed) > 0 {
		return nil, fmt.Errorf("not every reviewer finished, so the review was not stored: %s", strings.Join(failed, "; "))
	}
	return output, nil
}

// reviewVerdicts is as much of radar-gh's review (radar.PullRequestReview) as
// says whether each reviewer reviewed. With several reviewers Reviews holds
// each one's verdict and Agent their merge; a lone reviewer's is Agent.
type reviewVerdicts struct {
	Reviewer string          `json:"reviewer"`
	Agent    reviewerVerdict `json:"agent"`
	Reviews  []struct {
		Reviewer string          `json:"reviewer"`
		Agent    reviewerVerdict `json:"agent"`
	} `json:"reviews"`
}

// reviewerVerdict is as much of a radar.ACRResult as says whether the
// reviewer failed safe instead of reviewing, and why.
type reviewerVerdict struct {
	Summary string `json:"summary"`
	Failed  bool   `json:"failed"`
}

// maxFailureSummary bounds each failed reviewer's summary in the message: it
// carries a provider's error, which can run long.
const maxFailureSummary = 300

// failed names each reviewer that failed safe, with its summary. The merged
// verdict fails when any reviewer's does, so it is named only when no
// reviewer's verdict says which.
func (v reviewVerdicts) failed() []string {
	var out []string
	name := func(reviewer string, verdict reviewerVerdict) string {
		summary := verdict.Summary
		if len(summary) > maxFailureSummary {
			summary = strings.ToValidUTF8(summary[:maxFailureSummary], "") + "…"
		}
		return reviewer + ": " + summary
	}
	for _, r := range v.Reviews {
		if r.Agent.Failed {
			out = append(out, name(r.Reviewer, r.Agent))
		}
	}
	if len(out) == 0 && v.Agent.Failed {
		out = append(out, name(v.Reviewer, v.Agent))
	}
	return out
}

// drain waits for running reviews, then cancels what is left so each
// records a failed marker instead of leaving a running one to go stale.
func (s *server) drain(timeout time.Duration, cancel context.CancelFunc) {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
	finished := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return
	case <-time.After(timeout):
		s.logger.Warn("drain timed out; cancelling running reviews")
		cancel()
	}
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
	}
}

// summary is the tool's text content: the rendered review when there is one.
func summary(out reviewOutput) string {
	if out.Status == statusCompleted {
		if m, ok := out.Result.(map[string]any); ok {
			if md, ok := m["markdown"].(string); ok && md != "" {
				return md
			}
		}
		return fmt.Sprintf("Radar reviewed %s@%s; the structured result has the review.", out.Repository, out.SHA)
	}
	return fmt.Sprintf("%s@%s: %s. %s", out.Repository, out.SHA, out.Status, out.Message)
}

func actorFromRequest(req *mcp.CallToolRequest) (actor, error) {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return actor{}, errors.New("an authenticated identity is required")
	}
	h := req.Extra.Header
	a := actor{
		Email:     strings.TrimSpace(h.Get(actorEmailHeader)),
		Subject:   strings.TrimSpace(h.Get(actorSubjectHeader)),
		RequestID: strings.TrimSpace(h.Get(requestIDHeader)),
	}
	if a.Email == "" || a.Subject == "" {
		return actor{}, errors.New("an authenticated identity is required")
	}
	return a, nil
}

// proxyTokenHeader carries a secret only the auth proxy and this container
// hold (the proxy adds it with PROXY_HEADERS). The MCP listener is on pod
// loopback, which the reviewer container shares; without the secret, a
// process there could call it with any identity it liked.
const proxyTokenHeader = "X-Radar-MCP-Proxy-Token"

// requireIdentity rejects requests that did not come through the auth proxy
// with a verified identity, before the MCP layer sees them.
func requireIdentity(proxyToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exactly one value: the proxy adds its header to whatever the
		// caller sent, so a caller-supplied value shows up as a second one.
		got := r.Header.Values(proxyTokenHeader)
		if len(got) != 1 || subtle.ConstantTimeCompare([]byte(got[0]), []byte(proxyToken)) != 1 {
			http.Error(w, "requests must come through the auth proxy", http.StatusUnauthorized)
			return
		}
		if strings.TrimSpace(r.Header.Get(actorEmailHeader)) == "" || strings.TrimSpace(r.Header.Get(actorSubjectHeader)) == "" {
			http.Error(w, "an authenticated identity is required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handler() http.Handler {
	return requireIdentity(s.proxyToken, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp }, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		MaxRequestBodyBytes:          1 << 20,
		PropagateRequestCancellation: true,
	}))
}
