package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The reviewer container runs radar-gh and, through it, Codex. Codex runs as
// the container's user in a read-only sandbox and can read any file that user
// can, so this container must hold nothing worth reading: no IRSA web
// identity token, no Kubernetes service account token, no GitHub App key, and
// no credentials in its environment. Reviews get what they need per request
// from the server container: a one-repository read-only GitHub token and a
// credentials URL that serves Bedrock-only session credentials (broker.go).
//
// isolationCheck proves that from inside the container. The runner refuses to
// start when it fails, and the reviewer container's readiness probe runs it
// (radar-mcp check-isolation), so a chart change that mounts a secret into
// the reviewer takes the pod out of service instead of exposing the secret.

// defaultForbiddenPaths are where Kubernetes and the EKS pod identity webhook
// put credentials. The chart adds the GitHub App key's path through
// $RADAR_MCP_FORBIDDEN_PATHS.
var defaultForbiddenPaths = []string{
	"/var/run/secrets/eks.amazonaws.com/serviceaccount/token",
	"/var/run/secrets/kubernetes.io/serviceaccount/token",
	"/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token",
}

// forbiddenEnv are variables that would hand a process in the reviewer
// container a credential, point the SDKs at a credential file mounted
// anywhere, or name the server's secrets. A set variable fails the check
// whatever it points at: the reviewer needs none of them.
var forbiddenEnv = []string{
	"AWS_ROLE_ARN",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_SHARED_CREDENTIALS_FILE",
	"AWS_CONFIG_FILE",
	"AWS_BEARER_TOKEN_BEDROCK",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_PROFILE",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_CONTAINER_AUTHORIZATION_TOKEN",
	"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"GH_ENTERPRISE_TOKEN",
	"GITHUB_ENTERPRISE_TOKEN",
	"OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"FIREWORKS_API_KEY",
	"RADAR_MCP_GITHUB_APP_ID",
	"RADAR_MCP_GITHUB_APP_KEY_FILE",
	"RADAR_MCP_RESULTS_BUCKET",
	"RADAR_MCP_PROXY_TOKEN",
}

const defaultIMDS = "http://169.254.169.254"

type isolationCheck struct {
	paths  []string
	lookup func(string) (string, bool)
	// imds is the instance metadata endpoint. Reaching it would give the
	// node's role, so it must not answer from this pod.
	imds string
}

func newIsolationCheck() isolationCheck {
	paths := append([]string(nil), defaultForbiddenPaths...)
	for p := range strings.SplitSeq(os.Getenv("RADAR_MCP_FORBIDDEN_PATHS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return isolationCheck{paths: paths, lookup: os.LookupEnv, imds: defaultIMDS}
}

// homeCredentialFiles are where the AWS SDKs and gh read credentials with no
// variable set, under $HOME.
var homeCredentialFiles = []string{".aws/credentials", ".aws/config", ".config/gh/hosts.yml"}

// problems lists every way the container is not isolated; none means it is.
func (c isolationCheck) problems(ctx context.Context) []string {
	var out []string
	paths := c.paths
	if home, ok := c.lookup("HOME"); ok && home != "" {
		for _, f := range homeCredentialFiles {
			paths = append(paths, filepath.Join(home, f))
		}
	}
	for _, name := range forbiddenEnv {
		if v, ok := c.lookup(name); ok && v != "" {
			out = append(out, "the environment carries "+name)
		}
	}
	for _, p := range paths {
		f, err := os.Open(p)
		switch {
		case err == nil:
			f.Close()
			out = append(out, "can read "+p)
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
		default:
			// Fail closed: a path this cannot rule out counts as exposed.
			out = append(out, fmt.Sprintf("cannot tell whether %s is readable: %v", p, err))
		}
	}
	if c.imds != "" && imdsAnswers(ctx, c.imds) {
		out = append(out, "the instance metadata service answers at "+c.imds)
	}
	return out
}

// imdsAnswers reports whether IMDSv2 issues a token or IMDSv1 lists a role.
// Either would let a process reach the node's credentials. EKS nodes limit
// IMDSv2 to one hop, which stops it from a pod.
func imdsAnswers(ctx context.Context, base string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	try := func(method, path string, header map[string]string) bool {
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			return false
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	return try(http.MethodPut, "/latest/api/token", map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": "60"}) ||
		try(http.MethodGet, "/latest/meta-data/iam/security-credentials/", nil)
}

// runCheckIsolation is the reviewer container's readiness probe.
func runCheckIsolation() int {
	problems := newIsolationCheck().problems(context.Background())
	if len(problems) == 0 {
		fmt.Println("radar-mcp: reviewer container is isolated")
		return 0
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "radar-mcp: not isolated:", p)
	}
	return 1
}
