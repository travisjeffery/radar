package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

// The reviewer's credentials allow model inference and nothing else: not S3
// (the results bucket), not STS, not Secrets Manager. Allow-only, no
// NotAction, so the list is the whole grant before AWS intersects it with the
// role's.
func TestReviewSessionPolicyGrantsOnlyModelInference(t *testing.T) {
	var doc struct {
		Statement []struct {
			Effect    string          `json:"Effect"`
			Action    []string        `json:"Action"`
			NotAction json.RawMessage `json:"NotAction"`
			Resource  string          `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(reviewSessionPolicy), &doc); err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, st := range doc.Statement {
		if st.Effect != "Allow" || st.NotAction != nil {
			t.Fatalf("statement %+v: want Allow with Action only", st)
		}
		actions = append(actions, st.Action...)
	}
	sort.Strings(actions)
	want := []string{
		"aws-marketplace:Subscribe", "aws-marketplace:ViewSubscriptions",
		"bedrock-mantle:CreateInference",
		"bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream",
	}
	if strings.Join(actions, " ") != strings.Join(want, " ") {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
}

func testBroker() *credentialBroker {
	return newCredentialBroker(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "ASIAREVIEW", SecretAccessKey: "secret", SessionToken: "session",
			CanExpire: true, Expires: time.Now().Add(time.Hour)}, nil
	}))
}

func TestBrokerServesOnlyALiveReviewToken(t *testing.T) {
	b := testBroker()
	token, revoke := b.issue()
	get := func(auth string) int {
		req := httptest.NewRequest(http.MethodGet, "/credentials", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := get(""); got != http.StatusUnauthorized {
		t.Errorf("no token: %d", got)
	}
	if got := get("guess"); got != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", got)
	}
	if got := get(token); got != http.StatusOK {
		t.Errorf("live token: %d", got)
	}
	revoke()
	if got := get(token); got != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", got)
	}
}

// radar-gh loads credentials with the SDK's default chain; this is that chain
// reading the broker the way radar-gh's environment points it there.
func TestTheSDKReadsBrokerCredentials(t *testing.T) {
	b := testBroker()
	token, revoke := b.issue()
	defer revoke()
	srv := httptest.NewServer(b)
	defer srv.Close()
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", srv.URL+"/credentials")
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN", token)
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion("us-east-2"))
	if err != nil {
		t.Fatal(err)
	}
	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessKeyID != "ASIAREVIEW" || creds.SessionToken != "session" {
		t.Fatalf("creds = %+v", creds)
	}
}
