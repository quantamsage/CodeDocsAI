package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// fallbackModels support server-side refusal fallbacks (beta): if a safety
// classifier declines a request, the API re-runs it on a fallback model in
// the same call instead of returning nothing.
var fallbackModels = map[string]bool{
	"claude-opus-5-5": true, "claude-opus-5": true, "claude-sonnet-5-5": true,
	"claude-fable-5-1": true, "claude-fable-5-2": true,
}

// ClaudeConfig configures the Claude provider.
type ClaudeConfig struct {
	APIKey  string
	Model   string        // default "claude-opus-5-5"
	Effort  string        // low|medium|high|xhigh|max; default "medium"
	Timeout time.Duration // per request; default 5 minutes
	// MaxRetries is the SDK's own retry count for 408/429/5xx and network errors.
	MaxRetries int
	// BaseURL overrides the API endpoint (tests only).
	BaseURL string
}

// Claude is a Provider backed by the Claude API (official Go SDK).
type Claude struct {
	client anthropic.Client
	model  string
	effort anthropic.BetaOutputConfigEffort
}

// NewClaude returns a Claude provider, or an ErrNotConfigured error when no
// API key is given.
func NewClaude(cfg ClaudeConfig) (*Claude, error) {
	if cfg.APIKey == "" {
		return nil, &Error{Kind: ErrNotConfigured, Message: "No AI API key is configured (set ANTHROPIC_API_KEY)."}
	}
	if cfg.Model == "" {
		cfg.Model = "claude-opus-5-5"
	}
	if cfg.Effort == "" {
		cfg.Effort = "medium"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithRequestTimeout(cfg.Timeout),
		option.WithMaxRetries(cfg.MaxRetries),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &Claude{
		client: anthropic.NewClient(opts...),
		model:  cfg.Model,
		effort: anthropic.BetaOutputConfigEffort(cfg.Effort),
	}, nil
}

// Name implements Provider.
func (c *Claude) Name() string { return "claude" }

// Model implements Provider.
func (c *Claude) Model() string { return c.model }

// GenerateJSON implements Provider using schema-constrained output.
func (c *Claude) GenerateJSON(ctx context.Context, req Request) (Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: int64(maxTokens),
		System:    []anthropic.BetaTextBlockParam{{Text: req.System}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(req.Prompt))},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: c.effort,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: req.Schema},
		},
	}
	if fallbackModels[c.model] {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
	}

	msg, err := c.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return Response{}, classify(err)
	}
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return Response{}, &Error{Kind: ErrInvalidOutput, Message: "The AI service declined to process this part of the code."}
	case anthropic.BetaStopReasonMaxTokens:
		return Response{}, &Error{Kind: ErrInvalidOutput, Message: "The AI response was cut off before it was complete."}
	}
	var text string
	for _, block := range msg.Content {
		if block.Type == "text" {
			text = block.Text
			break
		}
	}
	if text == "" {
		return Response{}, &Error{Kind: ErrInvalidOutput, Message: "The AI service returned an empty response."}
	}
	if !json.Valid([]byte(text)) {
		return Response{}, &Error{Kind: ErrInvalidOutput, Message: "The AI service returned malformed data."}
	}
	return Response{
		JSON:         []byte(text),
		Model:        string(msg.Model),
		InputTokens:  int(msg.Usage.InputTokens),
		OutputTokens: int(msg.Usage.OutputTokens),
	}, nil
}

// classify maps SDK errors to error kinds.
func classify(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: ErrTransient, Message: "The AI service took too long to respond.", Err: err}
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return &Error{Kind: ErrTransient, Message: "Couldn't reach the AI service.", Err: err}
	}
	switch code := apiErr.StatusCode; {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return &Error{Kind: ErrNotConfigured, Message: "The AI service rejected the API key.", Err: err}
	case code == http.StatusTooManyRequests:
		return &Error{Kind: ErrRateLimited, Message: "The AI service is rate limiting requests.", RetryAfter: retryAfter(apiErr.Response), Err: err}
	case code == http.StatusRequestEntityTooLarge || code == http.StatusBadRequest || code == http.StatusNotFound:
		return &Error{Kind: ErrRequestRejected, Message: fmt.Sprintf("The AI service rejected the request (%d).", code), Err: err}
	case code >= 500 || code == http.StatusRequestTimeout || code == http.StatusConflict || code == 529:
		return &Error{Kind: ErrTransient, Message: fmt.Sprintf("The AI service is temporarily unavailable (%d).", code), Err: err}
	default:
		return &Error{Kind: ErrRequestRejected, Message: fmt.Sprintf("The AI service returned an error (%d).", code), Err: err}
	}
}

func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}
