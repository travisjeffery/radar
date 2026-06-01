package radar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// OpenAIAgent is an Automated Code Review agent backed by the OpenAI
// chat-completions API. Like LLMAgent (Anthropic) it implements ReviewAgent and
// is interchangeable with RuleBasedAgent; it reuses the shared ACR system prompt
// and verdict parsing, and fails SAFE (non-accept) on any error.
type OpenAIAgent struct {
	// APIKey authenticates to the OpenAI API. Defaults to $OPENAI_API_KEY.
	APIKey string
	// Model is the chat model. Defaults to $RADAR_ACR_MODEL or gpt-4o-mini.
	Model string
	// BaseURL is the chat-completions endpoint. Defaults to the public API.
	BaseURL string
	// HTTP is the client used for requests. Defaults to a 60s-timeout client.
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
		BaseURL: "https://api.openai.com/v1/chat/completions",
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

type openAIReq struct {
	Model          string          `json:"model"`
	Messages       []openAIMessage `json:"messages"`
	ResponseFormat *openAIRespFmt  `json:"response_format,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRespFmt struct {
	Type string `json:"type"`
}

type openAIResp struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Review sends the diff to the OpenAI model and returns the parsed verdict,
// failing safe (non-accept) on any error.
func (a *OpenAIAgent) Review(d Diff) ACRResult {
	res, err := a.review(context.Background(), d)
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
		url = "https://api.openai.com/v1/chat/completions"
	}

	reqBody := openAIReq{
		Model: a.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: acrSystemPrompt},
			{Role: "user", Content: renderDiffForReview(d)},
		},
		ResponseFormat: &openAIRespFmt{Type: "json_object"},
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
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ACRResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ACRResult{}, fmt.Errorf("openai API status %d: %s", resp.StatusCode, string(body))
	}

	var or openAIResp
	if err := json.Unmarshal(body, &or); err != nil {
		return ACRResult{}, err
	}
	if or.Error != nil {
		return ACRResult{}, fmt.Errorf("openai API error: %s", or.Error.Message)
	}
	if len(or.Choices) == 0 {
		return ACRResult{}, fmt.Errorf("openai API returned no choices")
	}
	return parseACRVerdict(or.Choices[0].Message.Content)
}
