package radar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// OpenAIAgent is an Automated Code Review agent backed by the OpenAI
// Responses API. Like LLMAgent (Anthropic) it implements ReviewAgent and
// is interchangeable with RuleBasedAgent; it reuses the shared ACR system prompt
// and verdict parsing, and fails SAFE (non-accept) on any error.
type OpenAIAgent struct {
	// APIKey authenticates to the OpenAI API. Defaults to $OPENAI_API_KEY.
	APIKey string
	// Model is the review model. Defaults to $RADAR_ACR_MODEL or gpt-4o-mini.
	Model string
	// BaseURL is the Responses endpoint. Defaults to the public API.
	BaseURL string
	// HTTP is the client used for requests. Defaults to a client with the
	// agent's Timeout. Review also enforces Timeout as a context deadline, so
	// a client without a Timeout cannot block the funnel forever.
	HTTP *http.Client
	// Timeout bounds one review; zero means reviewTimeout.
	Timeout time.Duration
	// ReasoningEffort, when set, is sent as reasoning.effort.
	ReasoningEffort string
	// MaxOutputTokens, when positive, caps the response; a response cut off
	// there is reported as truncated.
	MaxOutputTokens int
	// ExplicitCache sends prompt_cache_options {mode: explicit} with no
	// breakpoint. Radar's prefix is too short to be read back, so the default
	// implicit mode only bills cache writes, at 1.25x input, on every review.
	ExplicitCache bool
	// Provider prefixes Describe; empty means "openai".
	Provider string
}

// NewOpenAIAgent constructs an OpenAIAgent from the environment
// ($OPENAI_API_KEY, $RADAR_ACR_MODEL). It errors if no API key is available.
func NewOpenAIAgent() (*OpenAIAgent, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("radar: OPENAI_API_KEY not set")
	}
	model := os.Getenv("RADAR_ACR_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	timeout, err := acrTimeoutFromEnv(reviewTimeout)
	if err != nil {
		return nil, err
	}
	return &OpenAIAgent{
		APIKey:  key,
		Model:   model,
		BaseURL: "https://api.openai.com/v1/responses",
		HTTP:    &http.Client{Timeout: timeout},
		Timeout: timeout,
	}, nil
}

type openAIResponsesReq struct {
	Model              string              `json:"model"`
	Instructions       string              `json:"instructions"`
	Input              string              `json:"input"`
	Store              bool                `json:"store"`
	Text               openAITextConfig    `json:"text"`
	Reasoning          *openAIReasoning    `json:"reasoning,omitempty"`
	MaxOutputTokens    int                 `json:"max_output_tokens,omitempty"`
	PromptCacheOptions *openAICacheOptions `json:"prompt_cache_options,omitempty"`
}

type openAIReasoning struct {
	Effort string `json:"effort"`
}

type openAICacheOptions struct {
	Mode string `json:"mode"`
}

type openAITextConfig struct {
	Format openAITextFormat `json:"format"`
}

type openAITextFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type openAIResponsesResp struct {
	Status string `json:"status"`
	Usage  *struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens     int64 `json:"cached_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// acrVerdictSchema is the JSON Schema of the structured review, shared by
// every structured-output adapter (OpenAI, Fireworks, Bedrock, Codex). Every
// property is required, as strict structured output demands.
func acrVerdictSchema() map[string]any {
	str := map[string]any{"type": "string"}
	integer := func(min, max int) map[string]any {
		s := map[string]any{"type": "integer", "minimum": min}
		if max > 0 {
			s["maximum"] = max
		}
		return s
	}
	array := func(items map[string]any) map[string]any { return map[string]any{"type": "array", "items": items} }
	enum := func(values ...string) map[string]any { return map[string]any{"type": "string", "enum": values} }
	object := func(props map[string]any) map[string]any {
		required := make([]string, 0, len(props))
		for k := range props {
			required = append(required, k)
		}
		sort.Strings(required)
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	var safe, risk []string
	for s := range safeSignalSet {
		safe = append(safe, string(s))
	}
	for s := range riskSignalSet {
		risk = append(risk, string(s))
	}
	sort.Strings(safe)
	sort.Strings(risk)
	return object(map[string]any{
		"summary":       str,
		"risk":          object(map[string]any{"level": enum("low", "medium", "high"), "reason": str}),
		"where_to_look": array(object(map[string]any{"file": str, "line": integer(0, 0), "why": str})),
		"findings": array(object(map[string]any{
			"severity":       enum("P0", "P1", "P2", "P3"),
			"title":          str,
			"file":           str,
			"line":           integer(0, 0),
			"end_line":       integer(0, 0),
			"impact":         str,
			"recommendation": str,
			"rule_id":        str,
			"confidence":     integer(0, 10),
		})),
		"questions": array(str),
		"assumptions": array(object(map[string]any{
			"assumption":   str,
			"basis":        str,
			"if_wrong":     str,
			"load_bearing": map[string]any{"type": "boolean"},
		})),
		"tests_suggested": array(str),
		"signals":         object(map[string]any{"safe": array(enum(safe...)), "risk": array(enum(risk...))}),
		"coverage": object(map[string]any{
			"files_reviewed": map[string]any{"type": "array", "minItems": 1, "items": str},
			"repo_access":    map[string]any{"type": "boolean"},
			"limits":         str,
		}),
		"needs_human": object(map[string]any{"required": map[string]any{"type": "boolean"}, "reason": str}),
		"confidence":  integer(0, 10),
	})
}

// Review sends the diff to the OpenAI model and returns the parsed verdict,
// failing safe (non-accept) on any error.
func (a *OpenAIAgent) Review(d Diff) ACRResult {
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = reviewTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	res, err := a.review(ctx, d)
	if err != nil {
		res = ACRResult{Accept: false, Confidence: 0, Summary: "ACR OpenAI error, failing safe: " + err.Error(), Usage: res.Usage, Truncated: errors.Is(err, errOpenAITruncated), Failed: true}
	}
	elapsed := time.Since(start).Milliseconds()
	res.ElapsedMS = &elapsed
	return res
}

var errOpenAITruncated = errors.New("openai response truncated at max_output_tokens")

func (a *OpenAIAgent) review(ctx context.Context, d Diff) (ACRResult, error) {
	client := a.HTTP
	if client == nil {
		client = &http.Client{}
	}
	url := a.BaseURL
	if url == "" {
		url = "https://api.openai.com/v1/responses"
	}

	reqBody := openAIResponsesReq{
		Model:        a.Model,
		Instructions: ReviewSystemPrompt(d),
		Input:        renderDiffForReview(d),
		Store:        false,
		Text: openAITextConfig{
			Format: openAITextFormat{
				Type:   "json_schema",
				Name:   "radar_acr_verdict",
				Strict: true,
				Schema: acrVerdictSchema(),
			},
		},
		MaxOutputTokens: a.MaxOutputTokens,
	}
	if a.ReasoningEffort != "" {
		reqBody.Reasoning = &openAIReasoning{Effort: a.ReasoningEffort}
	}
	if a.ExplicitCache {
		reqBody.PromptCacheOptions = &openAICacheOptions{Mode: "explicit"}
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
	httpReq.Header.Set("authorization", "Bearer "+a.APIKey)

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
		return ACRResult{}, fmt.Errorf("openai API status %d: %s", resp.StatusCode, string(body))
	}

	var or openAIResponsesResp
	if err := json.Unmarshal(body, &or); err != nil {
		return ACRResult{}, err
	}
	if or.Error != nil {
		return ACRResult{}, fmt.Errorf("openai API error: %s", or.Error.Message)
	}
	var usage *ReviewUsage
	if or.Usage != nil {
		// Added to nothing, so the single request is also the first.
		usage = usage.add(&ReviewUsage{
			InputTokens: or.Usage.InputTokens, OutputTokens: or.Usage.OutputTokens,
			CacheReadTokens: or.Usage.InputTokensDetails.CachedTokens, CacheWriteTokens: or.Usage.InputTokensDetails.CacheWriteTokens,
			Requests: 1, PrefixSHA256: promptSHA256(ReviewSystemPrompt(d)),
		})
	}
	if or.Status != "completed" {
		reason := ""
		if or.IncompleteDetails != nil {
			reason = or.IncompleteDetails.Reason
		}
		if reason == "max_output_tokens" {
			return ACRResult{Usage: usage}, errOpenAITruncated
		}
		if reason != "" {
			reason = ": " + reason
		}
		return ACRResult{Usage: usage}, fmt.Errorf("openai response status %q%s", or.Status, reason)
	}

	var text strings.Builder
	refused := false
	for _, item := range or.Output {
		if item.Type != "message" {
			continue
		}
		for _, content := range item.Content {
			switch content.Type {
			case "output_text":
				text.WriteString(content.Text)
			case "refusal":
				refused = true
			}
		}
	}
	if text.Len() == 0 {
		if refused {
			return ACRResult{Usage: usage}, fmt.Errorf("openai API refused review")
		}
		return ACRResult{Usage: usage}, fmt.Errorf("openai API returned no output text")
	}
	res, err := parseACRVerdict(text.String())
	res.Usage = usage
	return res, err
}

// ConcurrentReviewSafe reports that each Review builds its own request and
// shares only the http.Client, which is safe for concurrent use.
func (a *OpenAIAgent) ConcurrentReviewSafe() bool { return true }

// Describe names the provider and model for decision provenance.
func (a *OpenAIAgent) Describe() string {
	provider := a.Provider
	if provider == "" {
		provider = "openai"
	}
	return provider + "/" + a.Model
}
