package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/travisjeffery/radar"
)

func TestExamplePolicyLoadsInShadowMode(t *testing.T) {
	policy, err := loadPullRequestPolicy(filepath.Join("..", "..", "examples", "github-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != radar.PullRequestModeShadow {
		t.Fatalf("example mode = %q, want shadow", policy.Mode)
	}
}

func TestEvaluateChecks(t *testing.T) {
	policy := radar.PullRequestPolicy{IgnoredChecks: []string{"github-actions/review"}}
	runs := []githubCheckRun{
		{ID: 1, Name: "test", Status: "completed", Conclusion: "failure"},
		{ID: 2, Name: "test", Status: "completed", Conclusion: "success"},
		{ID: 3, Name: "review", Status: "in_progress"},
	}
	runs[0].App.Slug = "github-actions"
	runs[1].App.Slug = "github-actions"
	runs[2].App.Slug = "github-actions"
	got := evaluateChecks(runs, nil, policy)
	if !got.Observed || !got.Passing || got.Fingerprint == "" {
		t.Fatalf("got %+v", got)
	}

	policy.IgnoredChecks = nil
	got = evaluateChecks(runs, nil, policy)
	if got.Passing {
		t.Fatal("in-progress self check must fail unless explicitly ignored")
	}

	got = evaluateChecks(nil, nil, policy)
	if got.Observed || got.Passing {
		t.Fatal("no checks must fail closed")
	}
}

func TestEvaluateChecksSkippedIsExplicit(t *testing.T) {
	run := githubCheckRun{ID: 1, Name: "optional", Status: "completed", Conclusion: "skipped"}
	run.App.Slug = "github-actions"
	policy := radar.PullRequestPolicy{}
	if evaluateChecks([]githubCheckRun{run}, nil, policy).Passing {
		t.Fatal("skipped check must fail by default")
	}
	policy.AllowSkippedChecks = true
	if !evaluateChecks([]githubCheckRun{run}, nil, policy).Passing {
		t.Fatal("skipped check should pass only when explicitly allowed")
	}
}

func TestPatchLineCountsMatch(t *testing.T) {
	patch := "@@ -1,2 +1,2 @@\n-old\n context\n+new"
	if !patchLineCountsMatch(patch, 1, 1) {
		t.Fatal("complete patch should match")
	}
	if patchLineCountsMatch(patch, 2, 1) {
		t.Fatal("truncated patch must not match provider totals")
	}
}

func TestExistingRadarApproval(t *testing.T) {
	changes := githubReview{ID: 1, State: "CHANGES_REQUESTED"}
	changes.User.Login = "reviewer"
	approved := githubReview{ID: 2, State: "APPROVED"}
	approved.User.Login = "reviewer"
	approved.User.Type = "Bot"
	approved.CommitID = strings.Repeat("a", 40)
	approved.Body = radarApprovalPrefix + "1 approved this exact head"
	existing := existingRadarApproval([]githubReview{changes, approved}, approved.CommitID)
	if !existing {
		t.Fatal("current Radar approval was not found")
	}
	if existingRadarApproval([]githubReview{approved}, strings.Repeat("b", 40)) {
		t.Fatal("approval for an old head must not be treated as current")
	}
}

func TestValidateApprovalSnapshot(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := radar.PullRequestInput{
		HeadSHA: head, CheckFingerprint: "stable", ChecksObserved: true, ChecksPassing: true,
		Open: true, SameRepository: true, ApprovalsKeptCurrent: true,
	}
	policy := radar.PullRequestPolicy{}
	if err := validateApprovalSnapshot(policy, base, base, head); err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Labels = []string{radar.DefaultHumanReviewLabel}
	if err := validateApprovalSnapshot(policy, base, changed, head); err == nil {
		t.Fatal("a human-review label added after review must block approval")
	}
	policy.HumanReviewLabel = "needs-human"
	if err := validateApprovalSnapshot(policy, base, changed, head); err != nil {
		t.Fatalf("only the policy's label counts: %v", err)
	}
	changed.Labels = []string{"needs-human"}
	if err := validateApprovalSnapshot(policy, base, changed, head); err == nil {
		t.Fatal("the policy's custom label must block approval")
	}
	changed = base
	changed.CheckFingerprint = "changed"
	if err := validateApprovalSnapshot(policy, base, changed, head); err == nil {
		t.Fatal("changed checks must block approval")
	}
	changed = base
	changed.BaseRef = "someone/feature"
	if err := validateApprovalSnapshot(policy, base, changed, head); err == nil {
		t.Fatal("a retarget after review must block approval")
	}
	changed = base
	changed.Stack = []radar.PullRequestStackEntry{{Number: 1, HeadRef: base.BaseRef, HeadSHA: head, BaseRef: "main"}}
	if err := validateApprovalSnapshot(policy, base, changed, head); err == nil {
		t.Fatal("a stacked pull request must never be approved")
	}
	changed = base
	changed.ApprovalsKeptCurrent = false
	if err := validateApprovalSnapshot(policy, base, changed, head); err == nil {
		t.Fatal("stale approval protection must block approval")
	}
}

func TestNewReviewAgentFireworks(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "")
	t.Setenv("RADAR_ACR_MODEL", "")
	if _, err := newReviewAgents("fireworks", agentOptions{}); err == nil || !strings.Contains(err.Error(), "FIREWORKS_API_KEY") {
		t.Fatalf("missing key must error, got %v", err)
	}
	t.Setenv("FIREWORKS_API_KEY", "test-key")
	if _, err := newReviewAgents("fireworks", agentOptions{}); err == nil || !strings.Contains(err.Error(), "RADAR_ACR_MODEL") {
		t.Fatalf("missing model must error, got %v", err)
	}
	t.Setenv("RADAR_ACR_MODEL", "accounts/fireworks/models/glm-5p3")
	agents, err := newReviewAgents("fireworks", agentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agents[0].(*radar.FireworksAgent); !ok {
		t.Fatalf("agent = %T, want *radar.FireworksAgent", agents[0])
	}
	if _, err := newReviewAgents("bogus", agentOptions{}); err == nil {
		t.Fatal("unknown agent must error")
	}
}

func TestNewReviewAgentsSeveral(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("FIREWORKS_API_KEY", "fireworks-key")
	t.Setenv("RADAR_ACR_MODEL", "accounts/fireworks/models/glm-5p3")
	if _, err := newReviewAgents("openai,fireworks=accounts/fireworks/models/glm-5p3", agentOptions{}); err == nil {
		t.Fatal("RADAR_ACR_MODEL must be unset with several agents")
	}
	t.Setenv("RADAR_ACR_MODEL", "")
	agents, err := newReviewAgents("openai, fireworks=accounts/fireworks/models/glm-5p3", agentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	if got := agents[0].(*radar.OpenAIAgent).Model; got != "gpt-4o-mini" {
		t.Fatalf("openai model = %q, want its default", got)
	}
	if got := agents[1].(*radar.FireworksAgent).Model; got != "accounts/fireworks/models/glm-5p3" {
		t.Fatalf("fireworks model = %q", got)
	}
	if _, err := newReviewAgents("openai,openai", agentOptions{}); err == nil {
		t.Fatal("duplicate agents must error")
	}
}

func stubPulls(t *testing.T, byHead map[string]string) *[]string {
	t.Helper()
	var calls []string
	prev := runGH
	t.Cleanup(func() { runGH = prev })
	runGH = func(args ...string) ([]byte, error) {
		endpoint := args[len(args)-1]
		calls = append(calls, endpoint)
		for head, body := range byHead {
			if strings.Contains(endpoint, "head="+url.QueryEscape("Acme:"+head)+"&") {
				return []byte(body), nil
			}
		}
		return []byte("[]"), nil
	}
	return &calls
}

func pullJSON(number int, head, base, headRepo string) string {
	return fmt.Sprintf(`{"number":%d,"head":{"ref":%q,"sha":"%040d","repo":{"full_name":%q}},"base":{"ref":%q,"repo":{"full_name":"Acme/backend"}}}`,
		number, head, number, headRepo, base)
}

func TestFetchStack(t *testing.T) {
	stubPulls(t, map[string]string{
		"dev/feature-1": "[" + pullJSON(23, "dev/feature-1", "dev/feature-0", "Acme/backend") + "]",
		"dev/feature-0": "[" + pullJSON(22, "dev/feature-0", "main", "Acme/backend") + "]",
	})
	stack, err := fetchStack("Acme/backend", "dev/feature-1", []string{"main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stack) != 2 || stack[0].Number != 23 || stack[1].Number != 22 || stack[1].BaseRef != "main" {
		t.Fatalf("stack = %+v", stack)
	}
}

func TestFetchStackRejectsForksAndAmbiguity(t *testing.T) {
	stubPulls(t, map[string]string{
		"fork-branch": "[" + pullJSON(1, "fork-branch", "main", "someone/backend") + "]",
		"shared":      "[" + pullJSON(2, "shared", "main", "Acme/backend") + "," + pullJSON(3, "shared", "release", "Acme/backend") + "]",
	})
	for _, base := range []string{"fork-branch", "shared", "no-such-branch"} {
		stack, err := fetchStack("Acme/backend", base, []string{"main"})
		if err != nil || stack != nil {
			t.Fatalf("base %s: stack=%+v err=%v, want no stack", base, stack, err)
		}
	}
}

func TestFetchStackIsBounded(t *testing.T) {
	pulls := map[string]string{}
	for i := range maxStackDepth + 2 {
		pulls[fmt.Sprintf("b%d", i)] = "[" + pullJSON(i+1, fmt.Sprintf("b%d", i), fmt.Sprintf("b%d", i+1), "Acme/backend") + "]"
	}
	calls := stubPulls(t, pulls)
	stack, err := fetchStack("Acme/backend", "b0", []string{"main"})
	if err != nil || stack != nil || len(*calls) != maxStackDepth {
		t.Fatalf("stack=%+v err=%v calls=%d", stack, err, len(*calls))
	}
}

func TestExampleReviewFindingsPolicyLoads(t *testing.T) {
	policy, err := loadPullRequestPolicy(filepath.Join("..", "..", "examples", "github-policy-review-findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if policy.ApprovalBasis != radar.ApprovalBasisReviewFindings || policy.Mode != radar.PullRequestModeShadow {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestSnapshotsStableIncludesTheStack(t *testing.T) {
	base := radar.PullRequestInput{HeadSHA: "h", CheckFingerprint: "c", BaseRef: "dev/a",
		Stack: []radar.PullRequestStackEntry{{Number: 1, HeadRef: "dev/a", HeadSHA: "p1", BaseRef: "main"}}}
	if !snapshotsStable(base, base) {
		t.Fatal("identical snapshots are stable")
	}
	moved := base
	moved.Stack = []radar.PullRequestStackEntry{{Number: 1, HeadRef: "dev/a", HeadSHA: "p2", BaseRef: "main"}}
	if snapshotsStable(base, moved) {
		t.Fatal("a parent force-push between observations must not count as stable")
	}
	dropped := base
	dropped.Stack = nil
	if snapshotsStable(base, dropped) {
		t.Fatal("a stack that disappears between observations must not count as stable")
	}
	retargeted := base
	retargeted.BaseRef = "main"
	if snapshotsStable(base, retargeted) {
		t.Fatal("a retarget between observations must not count as stable")
	}
}

func TestCheckoutFileResolvesFromThePolicyCheckout(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %v %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // a working directory outside the checkout
	got, err := checkoutFile(filepath.Join(repo, ".github", "radar-policy.json"), ".gitattributes")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(repo)
	if gotDir, _ := filepath.EvalSymlinks(filepath.Dir(got)); gotDir != want || filepath.Base(got) != ".gitattributes" {
		t.Fatalf("got %s, want %s/.gitattributes", got, want)
	}
	if _, err := checkoutFile(filepath.Join(t.TempDir(), "policy.json"), ".gitattributes"); err == nil {
		t.Fatal("a policy outside any checkout must be an error, not the working directory's file")
	}
}
func TestWriteAuditFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	if err := writeAuditFile(path, radar.AuthorTrustAudit{Tier: 2}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteAuditFileReplacesReadableFileAndRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeAuditFile(path, radar.AuthorTrustAudit{Tier: 2}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("existing world-readable audit not made private: %v %v", info.Mode(), err)
	}
	target := filepath.Join(dir, "elsewhere.json")
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if err := writeAuditFile(link, radar.AuthorTrustAudit{Tier: 2}); err == nil {
		t.Fatal("wrote through a symlink")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("symlink target was created")
	}
}

func TestApprovalsKeptCurrent(t *testing.T) {
	const (
		rulesetDismisses = `[[{"type":"pull_request","parameters":{"dismiss_stale_reviews_on_push":false}},{"type":"pull_request","parameters":{"dismiss_stale_reviews_on_push":true}}]]`
		rulesetKeeps     = `[[{"type":"deletion"},{"type":"pull_request","parameters":{"dismiss_stale_reviews_on_push":false}}]]`
		statusPinned     = `[[{"type":"pull_request","parameters":{"dismiss_stale_reviews_on_push":false}},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"PR Gate"},{"context":"radar/approval-current","integration_id":42}]}}]]`
		statusUnpinned   = `[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"radar/approval-current"}]}}]]`
		classicDismisses = `{"required_pull_request_reviews":{"dismiss_stale_reviews":true}}`
		classicKeeps     = `{"required_pull_request_reviews":{"dismiss_stale_reviews":false}}`
	)
	notProtected := errors.New("gh: Branch not protected (HTTP 404)")
	for _, tc := range []struct {
		name             string
		appID            int64
		rules, classic   string
		rulesErr, clsErr error
		want             bool
		wantErr          bool
	}{
		{name: "ruleset dismisses", rules: rulesetDismisses, clsErr: notProtected, want: true},
		{name: "approver status required", appID: 42, rules: statusPinned, clsErr: notProtected, want: true},
		{name: "status pinned to another App", appID: 7, rules: statusPinned, clsErr: notProtected},
		{name: "status required but no approver ID", rules: statusPinned, clsErr: notProtected},
		{name: "status not pinned to any App", appID: 42, rules: statusUnpinned, clsErr: notProtected},
		{name: "ruleset keeps, no classic protection", rules: rulesetKeeps, clsErr: notProtected},
		{name: "no rules, classic dismisses", rules: `[[]]`, classic: classicDismisses, want: true},
		{name: "ruleset keeps, classic keeps", rules: rulesetKeeps, classic: classicKeeps},
		{name: "rules unreadable", rulesErr: errors.New("boom"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := runGH
			t.Cleanup(func() { runGH = prev })
			runGH = func(args ...string) ([]byte, error) {
				endpoint := args[len(args)-1]
				switch {
				case strings.HasPrefix(endpoint, "repos/Acme/backend/rules/branches/main"):
					return []byte(tc.rules), tc.rulesErr
				case endpoint == "repos/Acme/backend/branches/main/protection":
					return []byte(tc.classic), tc.clsErr
				}
				t.Fatalf("unexpected gh call %v", args)
				return nil, nil
			}
			got, err := approvalsKeptCurrent("Acme/backend", "main", tc.appID)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %v, %v; want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
