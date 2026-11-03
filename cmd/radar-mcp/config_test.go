package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const exampleConfig = `{
  "repositories": {
    "Acme/backend": {
      "agent": "bedrock=us.anthropic.claude-opus-5-5,codex=openai.gpt-6.1-sol",
      "env": {"RADAR_BEDROCK_EFFORT": "high", "RADAR_CODEX_EFFORT": "high"}
    },
    "Acme/radar": {"agent": "bedrock=us.anthropic.claude-opus-5-5", "policy": ".github/radar.json"}
  },
  "env": {"AWS_REGION": "us-east-2", "RADAR_CODEX_REGION": "us-east-1", "RADAR_ACR_TIMEOUT": "780s"},
  "limits": {"review_wait": "30s"}
}`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadServerConfig(t *testing.T) {
	c, err := loadServerConfig(writeConfig(t, exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := c.settings("acme/BACKEND")
	if !ok || s.Repo != "Acme/backend" {
		t.Fatalf("settings = %+v, %v; want the canonical name, matched case-insensitively", s, ok)
	}
	if s.Policy != ".github/radar-policy.json" || s.Env["RADAR_CODEX_EFFORT"] != "high" || s.Env["AWS_REGION"] != "us-east-2" {
		t.Fatalf("settings = %+v; want the default policy and merged env", s)
	}
	if _, ok := c.settings("Acme/secret-repo"); ok {
		t.Fatal("a repository off the allowlist resolved")
	}
	l := c.Limits
	if l.ReviewWait.Duration != 30*time.Second || l.ReviewsPerHour != 10 || l.InFlightPerUser != 1 || l.ConcurrentReviews != 1 || l.ReviewTimeout.Duration != 45*time.Minute {
		t.Fatalf("limits = %+v", l)
	}
}

// The documented example must stay loadable.
func TestExampleConfigLoads(t *testing.T) {
	c, err := loadServerConfig("../../examples/radar-mcp-config.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.settings("your-org/app"); !ok {
		t.Fatal("the example does not allow your-org/app")
	}
}

func TestServerConfigRefusesCredentialsAndUnknownFields(t *testing.T) {
	for name, body := range map[string]string{
		"aws key":       `{"repositories":{"a/b":{"agent":"openai"}},"env":{"AWS_ACCESS_KEY_ID":"AKIA"}}`,
		"github token":  `{"repositories":{"a/b":{"agent":"openai","env":{"GH_TOKEN":"x"}}}}`,
		"codex binary":  `{"repositories":{"a/b":{"agent":"codex=openai.x","env":{"RADAR_CODEX_BIN":"/tmp/x"}}}}`,
		"server knob":   `{"repositories":{"a/b":{"agent":"openai"}},"env":{"RADAR_MCP_RESULTS_BUCKET":"x"}}`,
		"unknown field": `{"repositories":{"a/b":{"agent":"openai"}},"repos":{}}`,
		"no agent":      `{"repositories":{"a/b":{}}}`,
		"empty":         `{"repositories":{}}`,
		"bad policy":    `{"repositories":{"a/b":{"agent":"openai","policy":"/etc/x"}}}`,
		"duplicate":     `{"repositories":{"a/b":{"agent":"openai"},"A/B":{"agent":"openai"}}}`,
		"two at once":   `{"repositories":{"a/b":{"agent":"openai"}},"limits":{"concurrent_reviews":2}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadServerConfig(writeConfig(t, body)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestConfigDigestNamesTheReviewerConfiguration(t *testing.T) {
	c, err := loadServerConfig(writeConfig(t, exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := c.settings("Acme/backend")
	base := configDigest(s, "packs1")
	if base != configDigest(s, "packs1") {
		t.Fatal("digest is not stable")
	}
	changed := s
	changed.Env = map[string]string{"RADAR_CODEX_EFFORT": "low"}
	for name, d := range map[string]string{
		"packs":  configDigest(s, "packs2"),
		"env":    configDigest(changed, "packs1"),
		"agents": configDigest(reviewSettings{Agent: "openai", Policy: s.Policy, Env: s.Env}, "packs1"),
	} {
		if d == base {
			t.Errorf("changing %s kept the digest", name)
		}
	}
}

func TestDigestDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "security.md"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := digestDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "security.md"), []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := digestDir(dir)
	missing, err := digestDir(filepath.Join(dir, "absent"))
	if err != nil || a == b || strings.TrimSpace(missing) == "" {
		t.Fatalf("a=%s b=%s missing=%s err=%v", a, b, missing, err)
	}
}

// No ceiling on review_wait for now: a limit is to be chosen from recorded
// durations, so a long wait loads.
func TestReviewWaitHasNoCeiling(t *testing.T) {
	c, err := loadServerConfig(writeConfig(t, `{"repositories":{"a/b":{"agent":"openai"}},"limits":{"review_wait":"10m"}}`))
	if err != nil || c.Limits.ReviewWait.Duration != 10*time.Minute {
		t.Fatalf("10m: %v, %v", c, err)
	}
}

const wildcardConfig = `{
  "repositories": {
    "Acme/*": {"agent": "bedrock=us.anthropic.claude-opus-5-5,codex=openai.gpt-6.1-sol"},
    "Acme/backend": {"agent": "bedrock=us.anthropic.claude-opus-5-5,codex=openai.gpt-6.1-sol", "env": {"RADAR_CODEX_EFFORT": "high"}}
  }
}`

// An "owner/*" entry admits that owner's other repositories under Radar's
// default policy, in the caller's spelling until GitHub names them; an exact
// entry overrides it and is unchanged by it.
func TestWildcardEntries(t *testing.T) {
	c, err := loadServerConfig(writeConfig(t, wildcardConfig))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := c.settings("acme/BACKEND")
	if !ok || s.Repo != "Acme/backend" || s.Wildcard || s.DefaultPolicy || s.Env["RADAR_CODEX_EFFORT"] != "high" {
		t.Fatalf("exact entry = %+v, %v", s, ok)
	}
	s, ok = c.settings("acme/Radar")
	if !ok || s.Repo != "acme/Radar" || !s.Wildcard || !s.DefaultPolicy || s.Policy != ".github/radar-policy.json" {
		t.Fatalf("wildcard entry = %+v, %v", s, ok)
	}
	for _, repo := range []string{"Other/radar", "Acme/*", "Acme/..", "Acme/.", "Acme/-x", "Acme", "Acme/a/b", "Acme/a b"} {
		if _, ok := c.settings(repo); ok {
			t.Errorf("%q resolved", repo)
		}
	}
	if got := c.repositories(); strings.Join(got, ",") != "Acme/*,Acme/backend" {
		t.Errorf("repositories = %v", got)
	}
}

func TestWildcardConfigRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"one reviewer":  `{"repositories":{"a/*":{"agent":"openai"}}}`,
		"duplicate":     `{"repositories":{"a/*":{"agent":"openai,codex"},"A/*":{"agent":"openai,codex"}}}`,
		"every owner":   `{"repositories":{"*/*":{"agent":"openai,codex"}}}`,
		"partial glob":  `{"repositories":{"a/b*":{"agent":"openai,codex"}}}`,
		"bare wildcard": `{"repositories":{"*":{"agent":"openai,codex"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadServerConfig(writeConfig(t, body)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// Adding the default-policy field must not move the digest of a review that
// does not use it, or every stored backend review would run again.
func TestConfigDigestIsStableForExactEntries(t *testing.T) {
	c, err := loadServerConfig(writeConfig(t, exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := c.settings("Acme/backend")
	if got := configDigest(s, "packs"); got != "b5f0b035e221ba1d" {
		t.Fatalf("digest %s, want the one before wildcards, b5f0b035e221ba1d", got)
	}
	s.DefaultPolicy = true
	if configDigest(s, "packs") == "b5f0b035e221ba1d" {
		t.Fatal("the default policy did not change the digest")
	}
}
