package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Fake is a deterministic Provider for tests. Its Respond function decides
// each response from the request; calls are recorded.
type Fake struct {
	// Respond returns the value to serialize as JSON, or an error.
	Respond func(req Request) (any, error)

	mu    sync.Mutex
	calls []Request
}

// Name implements Provider.
func (f *Fake) Name() string { return "fake" }

// Model implements Provider.
func (f *Fake) Model() string { return "fake-model" }

// GenerateJSON implements Provider.
func (f *Fake) GenerateJSON(ctx context.Context, req Request) (Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if f.Respond == nil {
		return Response{}, fmt.Errorf("ai.Fake: no Respond function")
	}
	value, err := f.Respond(req)
	if err != nil {
		return Response{}, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return Response{}, err
	}
	return Response{JSON: data, Model: "fake-model", InputTokens: EstimateTokens(req.Prompt), OutputTokens: EstimateTokens(string(data))}, nil
}

// Calls returns the requests received so far.
func (f *Fake) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.calls...)
}
