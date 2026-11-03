package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// reviewSessionPolicy narrows the credentials the reviewer container gets to
// model inference. AWS intersects a session policy with the role's own, so
// the role's resource limits (which models, which regions) still apply, and
// whatever else the role can do (the results bucket) is out of reach: a
// reviewer, Codex included, that obtained these credentials could call
// Bedrock and nothing else. The actions are those CI's AI review role
// needs.
const reviewSessionPolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {"Sid": "BedrockInvoke", "Effect": "Allow", "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"], "Resource": "*"},
    {"Sid": "MantleInference", "Effect": "Allow", "Action": ["bedrock-mantle:CreateInference"], "Resource": "*"},
    {"Sid": "BedrockModelSubscribe", "Effect": "Allow", "Action": ["aws-marketplace:Subscribe", "aws-marketplace:ViewSubscriptions"], "Resource": "*"}
  ]
}`

// credentialBroker serves AWS credentials to radar-gh in the reviewer
// container over the pod's loopback interface, in the AWS SDKs' container
// credentials format ($AWS_CONTAINER_CREDENTIALS_FULL_URI with
// $AWS_CONTAINER_AUTHORIZATION_TOKEN). It holds the IRSA web identity; the
// reviewer container is not given one. Each review gets its own bearer
// token, revoked when the review ends, so a token outlives nothing.
type credentialBroker struct {
	creds aws.CredentialsProvider

	mu     sync.Mutex
	tokens map[string]struct{}
}

func newCredentialBroker(creds aws.CredentialsProvider) *credentialBroker {
	return &credentialBroker{creds: creds, tokens: map[string]struct{}{}}
}

// reviewCredentials assumes the pod's IRSA role again from its web identity
// token with reviewSessionPolicy attached. The SDK's default chain would hand
// out the role's full permissions; these are the reviewer's.
func reviewCredentials(cfg aws.Config) (aws.CredentialsProvider, error) {
	role, tokenFile := os.Getenv("AWS_ROLE_ARN"), os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
	if role == "" || tokenFile == "" {
		return nil, errors.New("AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE (IRSA) are required to vend reviewer credentials")
	}
	provider := stscreds.NewWebIdentityRoleProvider(sts.NewFromConfig(cfg), role, stscreds.IdentityTokenFile(tokenFile),
		func(o *stscreds.WebIdentityRoleOptions) {
			o.RoleSessionName = "radar-mcp-review"
			o.Policy = aws.String(reviewSessionPolicy)
		})
	return aws.NewCredentialsCache(provider), nil
}

// issue registers a token for one review and returns it with its revocation.
func (b *credentialBroker) issue() (string, func()) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := hex.EncodeToString(raw)
	b.mu.Lock()
	b.tokens[token] = struct{}{}
	b.mu.Unlock()
	return token, func() {
		b.mu.Lock()
		delete(b.tokens, token)
		b.mu.Unlock()
	}
}

func (b *credentialBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	b.mu.Lock()
	_, ok := b.tokens[r.Header.Get("Authorization")]
	b.mu.Unlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	creds, err := b.creds.Retrieve(ctx)
	if err != nil {
		http.Error(w, "credentials unavailable", http.StatusBadGateway)
		return
	}
	out := map[string]string{
		"AccessKeyId":     creds.AccessKeyID,
		"SecretAccessKey": creds.SecretAccessKey,
		"Token":           creds.SessionToken,
	}
	if creds.CanExpire {
		out["Expiration"] = creds.Expires.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
