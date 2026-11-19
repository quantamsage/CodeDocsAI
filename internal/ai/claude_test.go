package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var schema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
	"required":             []string{"ok"},
	"additionalProperties": false,
}

func message(text, stopReason string) string {
	content := []map[string]any{{"type": "thinking", "thinking": "", "signature": "x"}}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	body, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
		"content": content, "stop_reason": stopReason,
		"usage": map[string]any{"input_tokens": 120, "output_tokens": 30},
	})
	return string(body)
}

type captured struct {
	body   map[string]any
	header http.Header
}

func server(t *testing.T, status int, body string, headers map[string]string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got.body) //nolint:errcheck
		got.header = r.Header.Clone()
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func provider(t *testing.T, url, model string) *Claude {
	t.Helper()
	c, err := NewClaude(ClaudeConfig{APIKey: "test-key", Model: model, Effort: "medium", BaseURL: url, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClaudeSendsSchemaConstrainedRequest(t *testing.T) {
	srv, got := server(t, 200, message(`{"ok":true}`, "end_turn"), nil)
	resp, err := provider(t, srv.URL, "claude-opus-5-5").GenerateJSON(context.Background(), Request{
		System: "system rules", Prompt: "the prompt", Schema: schema, MaxTokens: 2000,
	})
	if err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	if string(resp.JSON) != `{"ok":true}` || resp.InputTokens != 120 || resp.OutputTokens != 30 || resp.Model != "claude-opus-5-5" {
		t.Errorf("response = %+v", resp)
	}

	b := got.body
	if b["model"] != "claude-opus-5-5" || b["max_tokens"] != float64(2000) {
		t.Errorf("model/max_tokens = %v/%v", b["model"], b["max_tokens"])
	}
	oc, _ := b["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if oc["effort"] != "medium" || format["type"] != "json_schema" || format["schema"] == nil {
		t.Errorf("output_config = %v", oc)
	}
	if b["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v", b["fallbacks"])
	}
	if !strings.Contains(got.header.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("anthropic-beta = %q", got.header.Get("Anthropic-Beta"))
	}
	if got.header.Get("X-Api-Key") != "test-key" && got.header.Get("Authorization") != "Bearer test-key" {
		t.Errorf("api key not sent: %v", got.header)
	}
	system, _ := b["system"].([]any)
	if len(system) != 1 || !strings.Contains(string(mustJSON(system)), "system rules") {
		t.Errorf("system = %v", system)
	}
	if _, ok := b["thinking"]; ok {
		t.Error("thinking must not be set for Claude Opus 5.5")
	}
}

func TestClaudeOmitsFallbacksForOtherModels(t *testing.T) {
	srv, got := server(t, 200, message(`{"ok":true}`, "end_turn"), nil)
	if _, err := provider(t, srv.URL, "claude-haiku-5-5").GenerateJSON(context.Background(), Request{Prompt: "p", Schema: schema}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.body["fallbacks"]; ok {
		t.Error("fallbacks sent for a model without support")
	}
}

func TestClaudeErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
		kind    error
		retry   bool
	}{
		{"refusal", 200, message("", "refusal"), nil, ErrInvalidOutput, true},
		{"cut off", 200, message(`{"ok":`, "max_tokens"), nil, ErrInvalidOutput, true},
		{"no text", 200, message("", "end_turn"), nil, ErrInvalidOutput, true},
		{"not json", 200, message("hello", "end_turn"), nil, ErrInvalidOutput, true},
		{"bad key", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, nil, ErrNotConfigured, false},
		{"rate limited", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, map[string]string{"Retry-After": "7", "X-Should-Retry": "false"}, ErrRateLimited, true},
		{"overloaded", 529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, map[string]string{"X-Should-Retry": "false"}, ErrTransient, true},
		{"server error", 500, `{"type":"error","error":{"type":"api_error","message":"boom"}}`, map[string]string{"X-Should-Retry": "false"}, ErrTransient, true},
		{"too large", 413, `{"type":"error","error":{"type":"request_too_large","message":"too big"}}`, nil, ErrRequestRejected, false},
		{"invalid", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, nil, ErrRequestRejected, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := server(t, tc.status, tc.body, tc.headers)
			_, err := provider(t, srv.URL, "claude-opus-5-5").GenerateJSON(context.Background(), Request{Prompt: "p", Schema: schema})
			if !errors.Is(err, tc.kind) {
				t.Fatalf("error = %v, want kind %v", err, tc.kind)
			}
			if Retryable(err) != tc.retry {
				t.Errorf("Retryable = %v, want %v", Retryable(err), tc.retry)
			}
			if UserMessage(err) == "" || strings.Contains(UserMessage(err), "test-key") {
				t.Errorf("user message = %q", UserMessage(err))
			}
		})
	}
}

func TestClaudeRateLimitRetryAfter(t *testing.T) {
	srv, _ := server(t, 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`, map[string]string{"Retry-After": "12", "X-Should-Retry": "false"})
	_, err := provider(t, srv.URL, "claude-opus-5-5").GenerateJSON(context.Background(), Request{Prompt: "p", Schema: schema})
	if RetryAfter(err) != 12*time.Second {
		t.Errorf("RetryAfter = %v", RetryAfter(err))
	}
}

func TestClaudeNetworkAndContextErrors(t *testing.T) {
	c := provider(t, "http://127.0.0.1:1", "claude-opus-5-5")
	if _, err := c.GenerateJSON(context.Background(), Request{Prompt: "p", Schema: schema}); !errors.Is(err, ErrTransient) {
		t.Errorf("unreachable server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GenerateJSON(ctx, Request{Prompt: "p", Schema: schema}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled context: %v", err)
	}
}

func TestNewClaudeRequiresKey(t *testing.T) {
	if _, err := NewClaude(ClaudeConfig{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

func TestFake(t *testing.T) {
	f := &Fake{Respond: func(req Request) (any, error) { return map[string]any{"echo": req.Label}, nil }}
	resp, err := f.GenerateJSON(context.Background(), Request{Label: "x", Prompt: "hello"})
	if err != nil || string(resp.JSON) != `{"echo":"x"}` || len(f.Calls()) != 1 {
		t.Errorf("fake = %s, %v, %d calls", resp.JSON, err, len(f.Calls()))
	}
}

func TestEstimateTokens(t *testing.T) {
	if n := EstimateTokens(strings.Repeat("a", 3500)); n < 900 || n > 1100 {
		t.Errorf("EstimateTokens = %d", n)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
