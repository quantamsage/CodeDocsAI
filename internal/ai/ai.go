// Package ai is the application's only gateway to a large language model.
//
// Everything else talks to the Provider interface: it sends a system prompt,
// a user prompt and a JSON schema, and receives JSON that matches the schema.
// Switching LLM vendors means adding another Provider implementation; no
// other package changes. Tests use Fake and never call a real API.
package ai

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Request is one structured-output call.
type Request struct {
	// System describes the task and the rules. Untrusted material (uploaded
	// source code) must never be placed here; it goes in Prompt, inside
	// clearly delimited data sections.
	System string
	Prompt string
	// Schema is the JSON schema the response must match.
	Schema map[string]any
	// MaxTokens bounds the response (including any reasoning). Zero means
	// the provider's default.
	MaxTokens int
	// Label names the call in logs and errors (e.g. "analyze batch 3").
	Label string
}

// Response is the model's JSON output and its token usage.
type Response struct {
	JSON         []byte
	Model        string
	InputTokens  int
	OutputTokens int
}

// Provider generates structured JSON with an LLM.
type Provider interface {
	// Name identifies the provider ("claude", "fake").
	Name() string
	// Model is the model used for calls, recorded with results.
	Model() string
	// GenerateJSON returns JSON matching req.Schema. Errors wrap one of the
	// sentinel errors below so callers can decide whether to retry.
	GenerateJSON(ctx context.Context, req Request) (Response, error)
}

// Error kinds.
var (
	// ErrNotConfigured: no credentials, or they were rejected. Not retried.
	ErrNotConfigured = errors.New("ai: provider not configured")
	// ErrRateLimited: the provider asked us to slow down. Retry later.
	ErrRateLimited = errors.New("ai: rate limited")
	// ErrTransient: timeouts, network and server errors. Retry later.
	ErrTransient = errors.New("ai: temporary failure")
	// ErrInvalidOutput: the response was cut off, refused or malformed.
	// Retrying the same request may succeed; splitting the input often does.
	ErrInvalidOutput = errors.New("ai: unusable response")
	// ErrRequestRejected: the request itself is invalid (e.g. too large).
	// Retrying it unchanged will not help.
	ErrRequestRejected = errors.New("ai: request rejected")
)

// Error carries an error kind, a message safe to show users, and an
// optional retry delay suggested by the provider.
type Error struct {
	Kind       error
	Message    string
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *Error) Unwrap() []error {
	if e.Err != nil {
		return []error{e.Kind, e.Err}
	}
	return []error{e.Kind}
}

// Retryable reports whether err is worth retrying later.
func Retryable(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrTransient) || errors.Is(err, ErrInvalidOutput)
}

// RetryAfter returns the provider's suggested delay, if any.
func RetryAfter(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

// UserMessage returns a message suitable for showing to users.
func UserMessage(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Message
	}
	return "The AI service failed."
}

// EstimateTokens is a conservative token estimate for budgeting requests:
// about one token per 3.5 bytes of source text.
func EstimateTokens(text string) int {
	return len(text)*2/7 + 1
}
