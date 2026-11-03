package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/travisjeffery/radar"
)

const (
	mantleService            = "bedrock-mantle"
	defaultCodexMaxTokens    = 32000
	defaultCodexEffort       = "high"
	codexProvider            = "bedrock-openai"
	mantleResponsesURLFormat = "https://bedrock-mantle.%s.api.aws/openai/v1/responses"
)

// NewCodexAgent reviews with an OpenAI model on Bedrock's Mantle endpoint,
// such as openai.gpt-5.6-terra, through the Responses API with the shared
// prompt and strict verdict schema. Requests are signed with SigV4 for
// bedrock-mantle from the AWS default credential chain and $AWS_REGION.
// Optional: $RADAR_CODEX_EFFORT (default high), $RADAR_CODEX_MAX_TOKENS
// (default 32000), $RADAR_ACR_TIMEOUT (default 480s).
func NewCodexAgent(ctx context.Context, model string) (*radar.OpenAIAgent, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("radar: the bedrock-openai agent needs a model, e.g. openai.gpt-5.6-terra")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		return nil, fmt.Errorf("radar: AWS_REGION is required for the bedrock-openai agent")
	}
	maxTokens := defaultCodexMaxTokens
	if raw := strings.TrimSpace(os.Getenv("RADAR_CODEX_MAX_TOKENS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("radar: RADAR_CODEX_MAX_TOKENS must be a positive integer, got %q", raw)
		}
		maxTokens = n
	}
	effort := defaultCodexEffort
	if raw := strings.TrimSpace(os.Getenv("RADAR_CODEX_EFFORT")); raw != "" {
		effort = raw
	}
	timeout := defaultTimeout
	if raw := strings.TrimSpace(os.Getenv("RADAR_ACR_TIMEOUT")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("radar: RADAR_ACR_TIMEOUT must be a positive duration such as 480s, got %q", raw)
		}
		timeout = d
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("radar: loading AWS config: %w", err)
	}
	return &radar.OpenAIAgent{
		Model:           model,
		BaseURL:         fmt.Sprintf(mantleResponsesURLFormat, region),
		HTTP:            &http.Client{Transport: &sigV4Transport{base: http.DefaultTransport, creds: cfg.Credentials, region: region}},
		Timeout:         timeout,
		ReasoningEffort: effort,
		MaxOutputTokens: maxTokens,
		ExplicitCache:   true,
		Provider:        codexProvider,
	}, nil
}

// sigV4Transport signs each request for bedrock-mantle; the signature
// replaces the empty bearer header the OpenAI agent sets.
type sigV4Transport struct {
	base   http.RoundTripper
	creds  aws.CredentialsProvider
	region string
	signer *v4.Signer
}

func (t *sigV4Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		_ = req.Body.Close()
	}
	signed := req.Clone(req.Context())
	signed.Body = io.NopCloser(bytes.NewReader(body))
	signed.ContentLength = int64(len(body))
	creds, err := t.creds.Retrieve(req.Context())
	if err != nil {
		return nil, fmt.Errorf("retrieving AWS credentials: %w", err)
	}
	sum := sha256.Sum256(body)
	signer := t.signer
	if signer == nil {
		signer = v4.NewSigner()
	}
	if err := signer.SignHTTP(req.Context(), creds, signed, hex.EncodeToString(sum[:]), mantleService, t.region, time.Now()); err != nil {
		return nil, fmt.Errorf("signing request: %w", err)
	}
	return t.base.RoundTrip(signed)
}
