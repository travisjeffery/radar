package radar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// LLMAgent is an Automated Code Review agent backed by the Anthropic Messages
// API. It feeds the diff's raw change content to Claude and asks it to classify
// the change against RADAR's safe/risk signal taxonomy, returning a structured
// verdict. It implements ReviewAgent and is interchangeable with RuleBasedAgent.
//
// It is never used unless explicitly constructed, so the default Engine, the
// build, and the test suite stay offline and deterministic. On any error
// (missing key, network failure, unparseable response) Review fails SAFE: it
// returns a non-accept verdict so the diff is routed to a human.
type LLMAgent struct {
	// APIKey authenticates to the Anthropic API. Defaults to $ANTHROPIC_API_KEY.
	APIKey string
	// Model is the Claude model ID. Defaults to $RADAR_ACR_MODEL or claude-opus-4-8.
	Model string
	// BaseURL is the Messages endpoint. Defaults to the public Anthropic API.
	BaseURL string
	// HTTP is the client used for requests. Defaults to a 60s-timeout client.
	// Review additionally enforces a reviewTimeout deadline per call, so a
	// caller-supplied client without a Timeout cannot block the funnel forever.
	HTTP *http.Client
}

// reviewTimeout bounds a single ACR API call regardless of the configured
// http.Client, so one hung request cannot wedge Engine.Classify indefinitely.
const reviewTimeout = 60 * time.Second

// NewLLMAgent constructs an LLMAgent from the environment ($ANTHROPIC_API_KEY,
// $RADAR_ACR_MODEL). It returns an error if no API key is available.
func NewLLMAgent() (*LLMAgent, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("radar: ANTHROPIC_API_KEY not set")
	}
	model := os.Getenv("RADAR_ACR_MODEL")
	if model == "" {
		model = "claude-opus-4-8"
	}
	return &LLMAgent{
		APIKey:  key,
		Model:   model,
		BaseURL: "https://api.anthropic.com/v1/messages",
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

const acrSystemPrompt = `You are RADAR's Automated Code Review (ACR) agent. Decide whether a code diff is safe to auto-land WITHOUT human review.

Classify the diff against these taxonomies.

SAFE signals (non-functional or low-risk): refactor-no-behavior-change, dead-code-removal, defensive-programming, logging-addition, pure-formatting, doc-comment-update, import-hygiene, test-addition, static-resource-update.

RISK signals (require a human): high-review-effort, structural-change, bug-or-logic-error, performance-risk, secrets-exposure, sql-injection, auth-bypass.

Auto-accept ONLY if your confidence is at least 8/10, every change has a recognized safe signal, there are zero risk signals, and there are no P0, P1, or P2 findings. If any of those conditions fail, do not accept.

Respond with ONLY a JSON object, no prose, of the form:
{"accept": bool, "confidence": int 0-10, "risk_signals": [string], "safe_signals": [string], "reviewed_files": [string], "findings": [{"severity":"P0|P1|P2|P3", "title":string, "file":string, "line":int, "summary":string}], "summary": string}

reviewed_files must contain every changed file path exactly once.`

// anthropic request/response shapes (minimal subset).
type anthropicReq struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Review sends the diff to Claude and returns the parsed verdict, failing safe
// (non-accept) on any error.
func (a *LLMAgent) Review(d Diff) ACRResult {
	ctx, cancel := context.WithTimeout(context.Background(), reviewTimeout)
	defer cancel()
	res, err := a.review(ctx, d)
	if err != nil {
		return ACRResult{Accept: false, Confidence: 0, Summary: "ACR LLM error, failing safe: " + err.Error()}
	}
	return res
}

func (a *LLMAgent) review(ctx context.Context, d Diff) (ACRResult, error) {
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	url := a.BaseURL
	if url == "" {
		url = "https://api.anthropic.com/v1/messages"
	}

	reqBody := anthropicReq{
		Model:     a.Model,
		MaxTokens: 1024,
		System:    acrSystemPrompt,
		Messages:  []anthropicMessage{{Role: "user", Content: renderDiffForReview(d)}},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return ACRResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return ACRResult{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(httpReq)
	if err != nil {
		return ACRResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ACRResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ACRResult{}, fmt.Errorf("anthropic API status %d: %s", resp.StatusCode, string(body))
	}

	var ar anthropicResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return ACRResult{}, err
	}
	if ar.Error != nil {
		return ACRResult{}, fmt.Errorf("anthropic API error: %s", ar.Error.Message)
	}

	var text strings.Builder
	for _, c := range ar.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return parseACRVerdict(text.String())
}

// renderDiffForReview formats a diff's changes as text for the LLM prompt.
func renderDiffForReview(d Diff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Diff %s (org %s, source %s)\n\n", d.ID, d.Org, d.Source)
	for i, c := range d.Changes {
		fmt.Fprintf(&b, "--- change %d: %s (complexity %d) ---\n%s\n\n", i+1, c.File, c.Complexity, c.Content)
	}
	return b.String()
}

// parseACRVerdict validates the model's strict JSON verdict and independently
// applies the conservative acceptance rule.
func parseACRVerdict(text string) (ACRResult, error) {
	text = strings.TrimSpace(text)
	var v struct {
		Accept        bool            `json:"accept"`
		Confidence    int             `json:"confidence"`
		RiskSignals   []string        `json:"risk_signals"`
		SafeSignals   []string        `json:"safe_signals"`
		ReviewedFiles []string        `json:"reviewed_files"`
		Findings      []ReviewFinding `json:"findings"`
		Summary       string          `json:"summary"`
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return ACRResult{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return ACRResult{}, fmt.Errorf("multiple JSON values in ACR response")
		}
		return ACRResult{}, fmt.Errorf("trailing content in ACR response: %w", err)
	}
	if v.Confidence < 0 || v.Confidence > 10 {
		return ACRResult{}, fmt.Errorf("ACR confidence outside 0-10")
	}
	if strings.TrimSpace(v.Summary) == "" {
		return ACRResult{}, fmt.Errorf("ACR summary is required")
	}
	res := ACRResult{
		Confidence:    v.Confidence,
		ReviewedFiles: v.ReviewedFiles,
		Findings:      v.Findings,
		Summary:       v.Summary,
	}
	for _, s := range v.RiskSignals {
		signal := ChangeSignal(s)
		if !isRiskSignal(signal) {
			return ACRResult{}, fmt.Errorf("unknown risk signal %q", s)
		}
		res.RiskSignals = append(res.RiskSignals, signal)
	}
	for _, s := range v.SafeSignals {
		signal := ChangeSignal(s)
		if !isSafeSignal(signal) {
			return ACRResult{}, fmt.Errorf("unknown safe signal %q", s)
		}
		res.SafeSignals = append(res.SafeSignals, signal)
	}
	blockingFinding := false
	for i := range res.Findings {
		f := &res.Findings[i]
		f.Severity = strings.ToUpper(strings.TrimSpace(f.Severity))
		if f.Severity != "P0" && f.Severity != "P1" && f.Severity != "P2" && f.Severity != "P3" {
			return ACRResult{}, fmt.Errorf("unknown finding severity %q", f.Severity)
		}
		if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Summary) == "" || f.Line < 0 {
			return ACRResult{}, fmt.Errorf("finding title, summary, and non-negative line are required")
		}
		if f.Severity != "P3" {
			blockingFinding = true
		}
	}
	// Enforce the acceptance criterion regardless of what the model claimed.
	res.Accept = v.Accept && v.Confidence >= ACRMinConfidence && len(res.RiskSignals) == 0 && len(res.SafeSignals) > 0 && len(res.ReviewedFiles) > 0 && !blockingFinding
	return res, nil
}
