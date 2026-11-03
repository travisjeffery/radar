package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/travisjeffery/radar"
)

const rulesFixture = `version: 1
repo_guidance: [CLAUDE.md]
areas:
  - name: go
    paths: ["**/*.go"]
    packs: [go]
  - name: iam
    paths: ["infra/pulumi/iam/**"]
    policy:
      auto_approve: false
`

func writeFile(t *testing.T, root, rel, data string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func criteriaCheckout(t *testing.T, rules string) (policyPath, packs string) {
	t.Helper()
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %v %s", err, out)
	}
	writeFile(t, repo, ".github/radar-policy.json", "{}")
	writeFile(t, repo, ".radar/rules.yaml", rules)
	writeFile(t, repo, "CLAUDE.md", "repo guide")
	writeFile(t, repo, "infra/AGENTS.md", "infra guide")
	writeFile(t, repo, "infra/notes.md", "not guidance")
	if out, err := exec.Command("git", "-C", repo, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	// Untracked, as a file the job wrote would be: never guidance.
	writeFile(t, repo, "svc/AGENTS.md", "untracked guide")
	packs = t.TempDir()
	writeFile(t, packs, "go.md", "---\nid: go\nversion: 2\n---\n- **GO-ERR-01** Wrap errors.\n")
	return filepath.Join(repo, ".github", "radar-policy.json"), packs
}

func criteriaPolicyFixture() radar.PullRequestPolicy {
	return radar.PullRequestPolicy{ReviewCriteria: &radar.PullRequestReviewCriteria{Rules: ".radar/rules.yaml"}}
}

func TestLoadReviewCriteria(t *testing.T) {
	policyPath, packs := criteriaCheckout(t, rulesFixture)
	t.Chdir(t.TempDir())
	c, err := loadReviewCriteria(policyPath, criteriaPolicyFixture(), packs)
	if err != nil {
		t.Fatal(err)
	}
	if c.Packs["go"].Version != 2 || len(c.Rules.Areas) != 2 || c.Rules.Areas[1].Policy.AutoApprove == nil || len(c.RulesSHA256) != 64 {
		t.Fatalf("criteria = %+v", c)
	}
	if len(c.Guidance) != 2 || string(c.Guidance["CLAUDE.md"]) != "repo guide" || string(c.Guidance["infra/AGENTS.md"]) != "infra guide" {
		t.Fatalf("guidance must be the tracked guidance files only: %v", keys(c.Guidance))
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestLoadReviewCriteriaFailsClosed(t *testing.T) {
	for name, tt := range map[string]struct {
		rules, packsDir string
		want            string
	}{
		"unknown key":   {rules: rulesFixture + "  - name: x\n    paths: [a]\n    polcy: {auto_approve: false}\n", want: "polcy"},
		"two documents": {rules: rulesFixture + "---\nversion: 2\n", want: "one YAML document"},
		"missing pack":  {rules: strings.Replace(rulesFixture, "[go]", "[python]", 1), want: "python"},
		"no packs dir":  {rules: rulesFixture, packsDir: "-", want: "-review-packs"},
		"approve true":  {rules: strings.Replace(rulesFixture, "auto_approve: false", "auto_approve: true", 1), want: "may only be false"},
	} {
		t.Run(name, func(t *testing.T) {
			policyPath, packs := criteriaCheckout(t, tt.rules)
			if tt.packsDir == "-" {
				packs = ""
			}
			_, err := loadReviewCriteria(policyPath, criteriaPolicyFixture(), packs)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestExampleRulesParse(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "radar-rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := parseReviewRules(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.Areas) != 3 || strings.Join(rules.PackIDs(), ",") != "go,iam-least-privilege,sql-migrations" {
		t.Fatalf("rules = %+v", rules)
	}
}
