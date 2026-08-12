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
	// HTTP is the client used for requests. Defaults to a 60s-timeout client.
	// Review additionally enforces a reviewTimeout deadline per call, so a
	// caller-supplied client without a Timeout cannot block the funnel forever.
	HTTP *http.Client
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
	return &OpenAIAgent{
		APIKey:  key,
		Model:   model,
		BaseURL: "https://api.openai.com/v1/responses",
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

type openAIResponsesReq struct {
	Model        string           `json:"model"`
	Instructions string           `json:"instructions"`
	Input        string           `json:"input"`
	Store        bool             `json:"store"`
	Text         openAITextConfig `json:"text"`
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
	Status            string `json:"status"`
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

func openAIACRVerdictSchema() map[string]any {
	stringArray := func(values ...string) map[string]any {
		items := map[string]any{"type": "string"}
		if len(values) > 0 {
			items["enum"] = values
		}
		return map[string]any{"type": "array", "items": items}
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"accept":     map[string]any{"type": "boolean"},
			"confidence": map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
			"risk_signals": stringArray(
				string(SignalHighReviewEffort),
				string(SignalStructuralChange),
				string(SignalBugOrLogicError),
				string(SignalPerformanceRisk),
				string(SignalSecretsExposure),
				string(SignalSQLInjection),
				string(SignalAuthBypass),
			),
			"safe_signals": stringArray(
				string(SignalRefactorNoBehaviorChange),
				string(SignalDeadCodeRemoval),
				string(SignalDefensiveProgramming),
				string(SignalLoggingAddition),
				string(SignalPureFormatting),
				string(SignalDocCommentUpdate),
				string(SignalImportHygiene),
				string(SignalTestAddition),
				string(SignalStaticResourceUpdate),
			),
			"reviewed_files": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items":    map[string]any{"type": "string"},
			},
			"findings": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"severity": map[string]any{"type": "string", "enum": []string{"P0", "P1", "P2", "P3"}},
						"title":    map[string]any{"type": "string"},
						"file":     map[string]any{"type": "string"},
						"line":     map[string]any{"type": "integer", "minimum": 0},
						"summary":  map[string]any{"type": "string"},
					},
					"required":             []string{"severity", "title", "file", "line", "summary"},
					"additionalProperties": false,
				},
			},
			"summary": map[string]any{"type": "string"},
		},
		"required": []string{
			"accept",
			"confidence",
			"risk_signals",
			"safe_signals",
			"reviewed_files",
			"findings",
			"summary",
		},
		"additionalProperties": false,
	}
}

// Review sends the diff to the OpenAI model and returns the parsed verdict,
// failing safe (non-accept) on any error.
func (a *OpenAIAgent) Review(d Diff) ACRResult {
	ctx, cancel := context.WithTimeout(context.Background(), reviewTimeout)
	defer cancel()
	res, err := a.review(ctx, d)
	if err != nil {
		return ACRResult{Accept: false, Confidence: 0, Summary: "ACR OpenAI error, failing safe: " + err.Error()}
	}
	return res
}

func (a *OpenAIAgent) review(ctx context.Context, d Diff) (ACRResult, error) {
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	url := a.BaseURL
	if url == "" {
		url = "https://api.openai.com/v1/responses"
	}

	reqBody := openAIResponsesReq{
		Model:        a.Model,
		Instructions: acrSystemPrompt,
		Input:        renderDiffForReview(d),
		Store:        false,
		Text: openAITextConfig{
			Format: openAITextFormat{
				Type:   "json_schema",
				Name:   "radar_acr_verdict",
				Strict: true,
				Schema: openAIACRVerdictSchema(),
			},
		},
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
	if or.Status != "completed" {
		reason := ""
		if or.IncompleteDetails != nil {
			reason = ": " + or.IncompleteDetails.Reason
		}
		return ACRResult{}, fmt.Errorf("openai response status %q%s", or.Status, reason)
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
			return ACRResult{}, fmt.Errorf("openai API refused review")
		}
		return ACRResult{}, fmt.Errorf("openai API returned no output text")
	}
	return parseACRVerdict(text.String())
}
