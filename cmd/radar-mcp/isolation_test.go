package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) (string, bool) { return "", false }

func TestIsolationCheckPassesWhenNothingIsExposed(t *testing.T) {
	dir := t.TempDir()
	c := isolationCheck{paths: []string{filepath.Join(dir, "absent")}, lookup: noEnv}
	if got := c.problems(context.Background()); len(got) != 0 {
		t.Fatalf("problems = %v, want none", got)
	}
}

// A secret the container can read fails the check: this is what catches a
// chart change that mounts the IRSA token or the App key into the reviewer.
func TestIsolationCheckFailsOnAReadableSecret(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("eyJ..."), 0o600); err != nil {
		t.Fatal(err)
	}
	c := isolationCheck{paths: []string{token}, lookup: noEnv}
	got := c.problems(context.Background())
	if len(got) != 1 || !strings.Contains(got[0], "can read "+token) {
		t.Fatalf("problems = %v, want the readable token", got)
	}
}

func TestIsolationCheckAcceptsAnUnreadableSecret(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	key := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(key, []byte("-----BEGIN"), 0o000); err != nil {
		t.Fatal(err)
	}
	c := isolationCheck{paths: []string{key}, lookup: noEnv}
	if got := c.problems(context.Background()); len(got) != 0 {
		t.Fatalf("problems = %v, want none for a file this user cannot read", got)
	}
}

func TestIsolationCheckFailsOnCredentialEnvironment(t *testing.T) {
	for _, name := range forbiddenEnv {
		t.Run(name, func(t *testing.T) {
			c := isolationCheck{lookup: func(k string) (string, bool) {
				if k == name {
					return "x", true
				}
				return "", false
			}}
			got := c.problems(context.Background())
			if len(got) != 1 || !strings.Contains(got[0], name) {
				t.Fatalf("problems = %v, want %s", got, name)
			}
		})
	}
}

func TestIsolationCheckCoversTheIRSAToken(t *testing.T) {
	c := newIsolationCheck()
	for _, want := range []string{"/var/run/secrets/eks.amazonaws.com/serviceaccount/token", "/var/run/secrets/kubernetes.io/serviceaccount/token"} {
		found := false
		for _, p := range c.paths {
			found = found || p == want
		}
		if !found {
			t.Errorf("default paths %v omit %s", c.paths, want)
		}
	}
	for _, want := range []string{"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "RADAR_MCP_GITHUB_APP_KEY_FILE", "RADAR_MCP_RESULTS_BUCKET"} {
		found := false
		for _, e := range forbiddenEnv {
			found = found || e == want
		}
		if !found {
			t.Errorf("forbidden environment omits %s", want)
		}
	}
}

func TestIsolationCheckAddsConfiguredPaths(t *testing.T) {
	t.Setenv("RADAR_MCP_FORBIDDEN_PATHS", "/etc/radar-mcp/github-app/private-key.pem, /x")
	c := newIsolationCheck()
	joined := strings.Join(c.paths, ",")
	if !strings.Contains(joined, "/etc/radar-mcp/github-app/private-key.pem") || !strings.Contains(joined, "/x") {
		t.Fatalf("paths = %v", c.paths)
	}
}

func TestIsolationCheckFailsWhenInstanceMetadataAnswers(t *testing.T) {
	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" {
			_, _ = w.Write([]byte("token"))
			return
		}
		http.NotFound(w, r)
	}))
	defer imds.Close()
	c := isolationCheck{lookup: noEnv, imds: imds.URL}
	got := c.problems(context.Background())
	if len(got) != 1 || !strings.Contains(got[0], "instance metadata") {
		t.Fatalf("problems = %v, want instance metadata", got)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	if got := (isolationCheck{lookup: noEnv, imds: url}).problems(context.Background()); len(got) != 0 {
		t.Fatalf("unreachable metadata: problems = %v, want none", got)
	}
}

// A credential file mounted anywhere and named by the SDKs' variables, or
// left at their default place under HOME, fails the check.
func TestIsolationCheckFailsOnCredentialFilesOutsideTheKnownPaths(t *testing.T) {
	for _, name := range []string{"AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE"} {
		c := isolationCheck{lookup: func(k string) (string, bool) {
			if k == name {
				return "/mnt/anywhere/credentials", true
			}
			return "", false
		}}
		if got := c.problems(context.Background()); len(got) != 1 || !strings.Contains(got[0], name) {
			t.Errorf("%s set: problems = %v", name, got)
		}
	}

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "credentials"), []byte("[default]\naws_access_key_id=AKIA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := isolationCheck{lookup: func(k string) (string, bool) {
		if k == "HOME" {
			return home, true
		}
		return "", false
	}}
	got := c.problems(context.Background())
	if len(got) != 1 || !strings.Contains(got[0], filepath.Join(home, ".aws", "credentials")) {
		t.Fatalf("credentials under HOME: problems = %v", got)
	}
}
