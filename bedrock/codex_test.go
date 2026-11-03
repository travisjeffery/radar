package bedrock

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestSigV4TransportSignsForMantleAndDropsBearer(t *testing.T) {
	var gotAuth, gotBody, gotDate string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotDate = r.Header.Get("Authorization"), r.Header.Get("X-Amz-Date")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer server.Close()
	transport := &sigV4Transport{
		base:   http.DefaultTransport,
		creds:  aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "token")),
		region: "us-east-2",
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer ")
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(gotAuth, "/us-east-2/bedrock-mantle/aws4_request") {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotBody != `{"model":"m"}` || gotDate == "" {
		t.Fatalf("body %q date %q", gotBody, gotDate)
	}
}

func TestNewCodexAgentConfiguration(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-2")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("RADAR_CODEX_MAX_TOKENS", "")
	t.Setenv("RADAR_CODEX_EFFORT", "")
	t.Setenv("RADAR_ACR_TIMEOUT", "")
	a, err := NewCodexAgent(context.Background(), "openai.gpt-5.6-terra")
	if err != nil {
		t.Fatal(err)
	}
	if a.BaseURL != "https://bedrock-mantle.us-east-2.api.aws/openai/v1/responses" || !a.ExplicitCache ||
		a.ReasoningEffort != "high" || a.MaxOutputTokens != 32000 || a.Describe() != "bedrock-openai/openai.gpt-5.6-terra" {
		t.Fatalf("agent = %+v describe=%q", a, a.Describe())
	}
	if _, err := NewCodexAgent(context.Background(), ""); err == nil {
		t.Fatal("a model is required")
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	if _, err := NewCodexAgent(context.Background(), "openai.gpt-5.6-terra"); err == nil {
		t.Fatal("a region is required")
	}
}
