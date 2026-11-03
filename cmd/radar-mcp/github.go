package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	errCommitNotFound = errors.New("commit not found")
	// errRepositoryNotFound is a repository the App cannot read: it does not
	// exist, or the App is not installed on it.
	errRepositoryNotFound = errors.New("repository not found")
)

// repositoryInfo is what GitHub says about a repository a wildcard admits.
type repositoryInfo struct {
	FullName string `json:"full_name"`
	Archived bool   `json:"archived"`
}

// commitSource is what the server needs from GitHub.
type commitSource interface {
	// reviewToken mints a token that can read only repo's contents, for one
	// review: fresh, so it outlives any review (an hour against a 45-minute
	// timeout).
	reviewToken(ctx context.Context, repo string) (string, error)
	// resolveCommit returns the full SHA of sha in repo.
	resolveCommit(ctx context.Context, repo, sha string) (string, error)
	// repository looks repo up through the App's installation.
	repository(ctx context.Context, repo string) (repositoryInfo, error)
}

// githubApp authenticates as the radar-mcp GitHub App. The App has
// Contents: read and Metadata: read and nothing else, so nothing this server
// does can write to GitHub; each token it mints is further narrowed to the one
// repository a review reads.
type githubApp struct {
	appID  string
	key    *rsa.PrivateKey
	api    string
	client *http.Client
	now    func() time.Time

	mu            sync.Mutex
	installations map[string]int64
	tokens        map[string]installationToken
}

type installationToken struct {
	token     string
	expiresAt time.Time
}

// tokenMinLifetime is the least time the server's cached token must have
// left; it is used only for single API calls. Reviews get their own.
const tokenMinLifetime = 10 * time.Minute

func loadGitHubApp(appID, keyFile string) (*githubApp, error) {
	if strings.TrimSpace(appID) == "" {
		return nil, errors.New("the GitHub App id is required")
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("reading the GitHub App key: %w", err)
	}
	key, err := parseRSAKey(b)
	if err != nil {
		return nil, err
	}
	return newGitHubApp(appID, key, "https://api.github.com"), nil
}

func newGitHubApp(appID string, key *rsa.PrivateKey, api string) *githubApp {
	return &githubApp{
		appID: strings.TrimSpace(appID), key: key, api: strings.TrimRight(api, "/"),
		client: &http.Client{Timeout: 30 * time.Second}, now: time.Now,
		installations: map[string]int64{}, tokens: map[string]installationToken{},
	}
}

func parseRSAKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("the GitHub App key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the GitHub App key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the GitHub App key is not an RSA key")
	}
	return key, nil
}

// jwt is the App's own credential, valid for nine minutes and backdated a
// minute for clock drift, as GitHub recommends.
func (g *githubApp) jwt() (string, error) {
	now := g.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": g.appID,
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// token returns the server's own token for repo, cached, for resolving
// commits.
func (g *githubApp) token(ctx context.Context, repo string) (string, error) {
	// Keyed case-insensitively, as GitHub names repositories, so a caller
	// cannot grow the cache by respelling one.
	key := strings.ToLower(repo)
	g.mu.Lock()
	cached, ok := g.tokens[key]
	g.mu.Unlock()
	if ok && cached.expiresAt.Sub(g.now()) > tokenMinLifetime {
		return cached.token, nil
	}
	t, err := g.mint(ctx, repo)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.tokens[key] = t
	g.mu.Unlock()
	return t.token, nil
}

func (g *githubApp) reviewToken(ctx context.Context, repo string) (string, error) {
	t, err := g.mint(ctx, repo)
	return t.token, err
}

func (g *githubApp) mint(ctx context.Context, repo string) (installationToken, error) {
	jwt, err := g.jwt()
	if err != nil {
		return installationToken{}, err
	}
	id, err := g.installation(ctx, repo, jwt)
	if err != nil {
		return installationToken{}, err
	}
	_, name, _ := strings.Cut(repo, "/")
	body, _ := json.Marshal(map[string]any{
		"repositories": []string{name},
		"permissions":  map[string]string{"contents": "read", "metadata": "read"},
	})
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), "Bearer "+jwt, body, &out); err != nil {
		// The App may have been reinstalled under a new id; look it up again next time.
		g.mu.Lock()
		delete(g.installations, strings.ToLower(repo))
		g.mu.Unlock()
		return installationToken{}, fmt.Errorf("minting a token for %s: %w", repo, err)
	}
	if out.Token == "" {
		return installationToken{}, fmt.Errorf("GitHub returned no token for %s", repo)
	}
	return installationToken{token: out.Token, expiresAt: out.ExpiresAt}, nil
}

func (g *githubApp) installation(ctx context.Context, repo, jwt string) (int64, error) {
	key := strings.ToLower(repo)
	g.mu.Lock()
	id, ok := g.installations[key]
	g.mu.Unlock()
	if ok {
		return id, nil
	}
	var out struct {
		ID int64 `json:"id"`
	}
	err := g.do(ctx, http.MethodGet, "/repos/"+repo+"/installation", "Bearer "+jwt, nil, &out)
	if isStatus(err, http.StatusNotFound) {
		return 0, fmt.Errorf("%w: %s does not exist or the radar-mcp App is not installed on it", errRepositoryNotFound, repo)
	}
	if err != nil {
		return 0, fmt.Errorf("finding the radar-mcp App's installation on %s: %w", repo, err)
	}
	g.mu.Lock()
	g.installations[key] = out.ID
	g.mu.Unlock()
	return out.ID, nil
}

func (g *githubApp) repository(ctx context.Context, repo string) (repositoryInfo, error) {
	token, err := g.token(ctx, repo)
	if err != nil {
		return repositoryInfo{}, err
	}
	var out repositoryInfo
	err = g.do(ctx, http.MethodGet, "/repos/"+repo, "token "+token, nil, &out)
	if isStatus(err, http.StatusNotFound) {
		return repositoryInfo{}, fmt.Errorf("%w: %s", errRepositoryNotFound, repo)
	}
	if err != nil {
		return repositoryInfo{}, err
	}
	if !repoPattern.MatchString(out.FullName) {
		return repositoryInfo{}, fmt.Errorf("GitHub named %s %q", repo, out.FullName)
	}
	return out, nil
}

func isStatus(err error, code int) bool {
	var status *githubStatusError
	return errors.As(err, &status) && status.code == code
}

func (g *githubApp) resolveCommit(ctx context.Context, repo, sha string) (string, error) {
	token, err := g.token(ctx, repo)
	if err != nil {
		return "", err
	}
	var out struct {
		SHA string `json:"sha"`
	}
	err = g.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+sha, "token "+token, nil, &out)
	var status *githubStatusError
	if errors.As(err, &status) && (status.code == http.StatusNotFound || status.code == http.StatusUnprocessableEntity) {
		return "", fmt.Errorf("%w: %s is not a commit pushed to %s", errCommitNotFound, sha, repo)
	}
	if err != nil {
		return "", err
	}
	if !fullSHA.MatchString(out.SHA) {
		return "", fmt.Errorf("GitHub returned %q for %s", out.SHA, sha)
	}
	return out.SHA, nil
}

type githubStatusError struct {
	code int
	body string
}

func (e *githubStatusError) Error() string { return fmt.Sprintf("GitHub %d: %s", e.code, e.body) }

func (g *githubApp) do(ctx context.Context, method, path, auth string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &githubStatusError{code: resp.StatusCode, body: msg}
	}
	return json.Unmarshal(b, out)
}
