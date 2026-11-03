package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	// repoPattern is GitHub's naming: an owner never starts with a hyphen and
	// neither part starts with one, so a name can never read as a flag.
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.][A-Za-z0-9_.-]*$`)
	// wildcardPattern is an "owner/*" entry: every repository the owner has.
	wildcardPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/\*$`)
	shortSHA        = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	fullSHA         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	reviewEnvValue  = regexp.MustCompile(`^[A-Za-z0-9_.:/=,+-]*$`)
)

// serverConfig is what CI keeps in each repository's caller workflow: which
// repositories may be reviewed and with which reviewers. It is mounted from
// the deployment, so changing a reviewer is a deployment change, as it is a
// workflow change in CI.
type serverConfig struct {
	// Repositories is the allowlist. Only repositories every engineer can
	// already read belong here: any signed-in user can review any of
	// them, and findings quote code.
	//
	// An "owner/*" entry admits every repository of that organization, so
	// it belongs here only while the organization's base permission lets
	// every member read every repository (see DECISIONS.md).
	// An exact entry overrides the wildcard for its repository.
	Repositories map[string]repoConfig `json:"repositories"`
	// Env sets Radar's knobs for every repository, as CI's review step does
	// (RADAR_ACR_TIMEOUT, RADAR_CODEX_REGION, AWS_REGION, ...).
	Env    map[string]string `json:"env,omitempty"`
	Limits limitsConfig      `json:"limits"`

	byLower map[string]string
	// wildcards maps a lowercased owner to its "owner/*" entry.
	wildcards map[string]string
}

type repoConfig struct {
	// Agent is radar-gh's -agent, as the repository's CI passes it.
	Agent string `json:"agent"`
	// Policy is the policy path, read at the base branch's tip.
	Policy string `json:"policy,omitempty"`
	// Env overrides the shared Env for this repository (efforts, models).
	Env map[string]string `json:"env,omitempty"`
}

type limitsConfig struct {
	// ReviewsPerHour is how many new reviews one person may start an hour.
	// Stored results and status checks are free.
	ReviewsPerHour int `json:"reviews_per_hour"`
	// InFlightPerUser is how many of one person's reviews may run at once.
	InFlightPerUser int `json:"in_flight_per_user"`
	// ConcurrentReviews caps reviews per pod. It must be 1: reviews in one
	// reviewer container share its user and process namespace, so one
	// review's Codex could read another's GitHub and broker tokens from the
	// short-lived gh and git processes that carry them. Scale with replicas.
	ConcurrentReviews int `json:"concurrent_reviews"`
	// ReviewWait is how long radar_review waits for a result before
	// answering "running"; a review takes minutes, longer than clients wait
	// for one tool call. No ceiling yet: radar_mcp_tool_call_duration_seconds
	// and radar_mcp_review_duration_seconds are the data to choose one from.
	ReviewWait duration `json:"review_wait"`
	// ReviewTimeout bounds one review end to end. It must outlast Radar's own
	// reviewer deadlines (RADAR_ACR_TIMEOUT, RADAR_CODEX_TIMEOUT).
	ReviewTimeout duration `json:"review_timeout"`
}

// reviewSettings are the resolved settings one review runs with.
type reviewSettings struct {
	Repo   string            `json:"repo"`
	Agent  string            `json:"agent"`
	Policy string            `json:"policy"`
	Env    map[string]string `json:"env"`
	// DefaultPolicy reviews under Radar's built-in policy when the
	// repository has no file at Policy. Wildcard repositories have no CI
	// caller workflow, so most have no policy of their own.
	DefaultPolicy bool `json:"default_policy,omitempty"`
	// Wildcard is set when an "owner/*" entry admitted the repository: Repo
	// is the caller's spelling until GitHub names it.
	Wildcard bool `json:"-"`
}

type duration struct{ time.Duration }

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func loadServerConfig(path string) (*serverConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c serverConfig
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *serverConfig) validate() error {
	if len(c.Repositories) == 0 {
		return errors.New("repositories is empty")
	}
	if err := validateReviewEnv(c.Env); err != nil {
		return fmt.Errorf("env: %w", err)
	}
	c.byLower = make(map[string]string, len(c.Repositories))
	c.wildcards = map[string]string{}
	for name, repo := range c.Repositories {
		lower := strings.ToLower(name)
		switch {
		case wildcardPattern.MatchString(name):
			owner, _, _ := strings.Cut(lower, "/")
			if _, dup := c.wildcards[owner]; dup {
				return fmt.Errorf("repository %q is listed twice", name)
			}
			c.wildcards[owner] = name
			// Wildcard repositories fall back to Radar's default policy, which
			// requires every reviewer to pass; one reviewer cannot be a
			// second opinion.
			if len(strings.Split(repo.Agent, ",")) < 2 {
				return fmt.Errorf("repository %s: a wildcard entry needs at least two reviewers, as backend's CI runs", name)
			}
		case repoPattern.MatchString(name):
			if _, dup := c.byLower[lower]; dup {
				return fmt.Errorf("repository %q is listed twice", name)
			}
			c.byLower[lower] = name
		default:
			return fmt.Errorf("repository %q must be owner/repository or owner/*", name)
		}
		if strings.TrimSpace(repo.Agent) == "" {
			return fmt.Errorf("repository %s: agent is required", name)
		}
		if !reviewEnvValue.MatchString(repo.Agent) {
			return fmt.Errorf("repository %s: agent %q has characters radar-gh does not take", name, repo.Agent)
		}
		if repo.Policy != "" && !filepath.IsLocal(repo.Policy) {
			return fmt.Errorf("repository %s: policy must be a relative path inside the repository", name)
		}
		if err := validateReviewEnv(repo.Env); err != nil {
			return fmt.Errorf("repository %s env: %w", name, err)
		}
	}
	l := &c.Limits
	if l.ReviewsPerHour == 0 {
		l.ReviewsPerHour = 10
	}
	if l.InFlightPerUser == 0 {
		l.InFlightPerUser = 1
	}
	if l.ConcurrentReviews == 0 {
		l.ConcurrentReviews = 1
	}
	if l.ConcurrentReviews != 1 {
		return errors.New("limits.concurrent_reviews must be 1: one review per reviewer container keeps reviews from reading each other's credentials; add replicas instead")
	}
	if l.ReviewWait.Duration == 0 {
		l.ReviewWait.Duration = 45 * time.Second
	}
	if l.ReviewTimeout.Duration == 0 {
		l.ReviewTimeout.Duration = 45 * time.Minute
	}
	if l.ReviewsPerHour < 0 || l.InFlightPerUser < 0 || l.ConcurrentReviews < 0 || l.ReviewWait.Duration < 0 || l.ReviewTimeout.Duration < 0 {
		return errors.New("limits must be positive")
	}
	return nil
}

// validateReviewEnv admits only Radar's own knobs and the region, so the
// config cannot hand the reviewer a credential or redirect its tools.
func validateReviewEnv(env map[string]string) error {
	for k, v := range env {
		if !allowedReviewEnv(k) {
			return fmt.Errorf("%s is not a Radar setting (RADAR_*, AWS_REGION or AWS_DEFAULT_REGION)", k)
		}
		if !reviewEnvValue.MatchString(v) {
			return fmt.Errorf("%s has characters a Radar setting does not use", k)
		}
	}
	return nil
}

func allowedReviewEnv(k string) bool {
	switch k {
	case "AWS_REGION", "AWS_DEFAULT_REGION":
		return true
	case "RADAR_CODEX_BIN":
		// The Codex binary is the image's; the config does not choose what runs.
		return false
	}
	return strings.HasPrefix(k, "RADAR_") && !strings.HasPrefix(k, "RADAR_MCP_")
}

// settings resolves repo, case-insensitively as GitHub does, to its
// canonical name and review settings. An exact entry wins over its owner's
// wildcard. A repository a wildcard admits keeps the caller's spelling and
// is marked Wildcard: the server must still confirm it with GitHub.
func (c *serverConfig) settings(repo string) (reviewSettings, bool) {
	lower := strings.ToLower(repo)
	name, ok := c.byLower[lower]
	wildcard := false
	if !ok {
		owner, rest, _ := strings.Cut(lower, "/")
		// repoPattern keeps the name out of flags and URL paths; "." and ".."
		// are not repositories.
		if name, ok = c.wildcards[owner]; !ok || !repoPattern.MatchString(repo) || rest == "." || rest == ".." {
			return reviewSettings{}, false
		}
		wildcard = true
	}
	rc := c.Repositories[name]
	env := make(map[string]string, len(c.Env)+len(rc.Env))
	maps.Copy(env, c.Env)
	maps.Copy(env, rc.Env)
	policy := rc.Policy
	if policy == "" {
		policy = ".github/radar-policy.json"
	}
	if wildcard {
		return reviewSettings{Repo: repo, Agent: rc.Agent, Policy: policy, Env: env, DefaultPolicy: true, Wildcard: true}, true
	}
	return reviewSettings{Repo: name, Agent: rc.Agent, Policy: policy, Env: env}, true
}

func (c *serverConfig) repositories() []string {
	names := make([]string, 0, len(c.Repositories))
	for name := range c.Repositories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// configDigest names a reviewer configuration: the same commit reviewed by
// different reviewers, settings or pack text is a different review, so it is
// stored under a different key.
func configDigest(s reviewSettings, packsDigest string) string {
	b, _ := json.Marshal(struct {
		Agent  string            `json:"agent"`
		Policy string            `json:"policy"`
		Env    map[string]string `json:"env"`
		Packs  string            `json:"packs"`
		// Omitted when false, so exact entries keep their stored reviews.
		DefaultPolicy bool `json:"default_policy,omitempty"`
	}{s.Agent, s.Policy, s.Env, packsDigest, s.DefaultPolicy})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// digestDir hashes every file's path and contents under dir, so a change to
// any review pack changes the configuration digest. A missing dir hashes as
// empty.
func digestDir(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == dir {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}
