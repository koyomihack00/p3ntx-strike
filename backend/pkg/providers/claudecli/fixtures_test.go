package claudecli

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"pentagi/pkg/providers/anthropic"
	"pentagi/pkg/tools"

	"github.com/vxcontrol/langchaingo/llms"
)

// fakeRunner stands in for the worker. Its reply is computed per call, and it
// returns ctx.Err() once the context is done, as a real worker must.
type fakeRunner struct {
	mu       sync.Mutex
	requests []Request
	reply    func(ctx context.Context, call int, req Request) (*Result, error)
}

func (f *fakeRunner) Run(ctx context.Context, req Request) (*Result, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	call := len(f.requests)
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res, err := f.reply(ctx, call, req)
	if ctxErr := ctx.Err(); ctxErr != nil && err == nil {
		return nil, ctxErr
	}
	return res, err
}

func (f *fakeRunner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeRunner) lastRequest(t *testing.T) Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("runner was never called")
	}
	return f.requests[len(f.requests)-1]
}

func replyWith(res *Result) func(context.Context, int, Request) (*Result, error) {
	return func(context.Context, int, Request) (*Result, error) { return res, nil }
}

// blockUntilDone simulates a CLI that never answers.
func blockUntilDone(ctx context.Context, _ int, _ Request) (*Result, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func structured(output string) *Result {
	return &Result{
		Subtype:          "success",
		StructuredOutput: json.RawMessage(output),
		NumTurns:         2,
		InitTools:        []string{"StructuredOutput"},
		ToolUses:         []string{"StructuredOutput"},
		Usage: Usage{
			InputTokens:              1200,
			OutputTokens:             80,
			CacheReadInputTokens:     300,
			CacheCreationInputTokens: 40,
		},
		TotalCostUSD: 0.0123,
		CLIVersion:   "test",
	}
}

func newTestProvider(t *testing.T, runner Runner, mutate ...func(*Options)) *Provider {
	t.Helper()
	cfg, err := anthropic.DefaultProviderConfig()
	if err != nil {
		t.Fatalf("anthropic config: %v", err)
	}
	models, err := anthropic.DefaultModels()
	if err != nil {
		t.Fatalf("anthropic models: %v", err)
	}
	opts := Options{
		Name:               "claude-cli-test",
		Config:             cfg,
		Models:             models,
		DefaultModel:       "claude-sonnet-5",
		ToolCallIDTemplate: "toolu_{r:24:b}",
		Runner:             runner,
		CallTimeout:        30 * time.Second,
	}
	for _, m := range mutate {
		m(&opts)
	}
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// pentagiTools returns real PentAGI tool definitions, so schemas are the shipped ones.
func pentagiTools(t *testing.T, names ...string) []llms.Tool {
	t.Helper()
	defs := tools.GetRegistryDefinitions()
	out := make([]llms.Tool, 0, len(names))
	for _, name := range names {
		def, ok := defs[name]
		if !ok {
			t.Fatalf("no registry definition for %q", name)
		}
		out = append(out, llms.Tool{Type: "function", Function: &def})
	}
	return out
}

func taskChain() []llms.MessageContent {
	return []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, "You are the pentester agent."),
		llms.TextParts(llms.ChatMessageTypeHuman, "Find out which user the shell runs as."),
	}
}

const terminalCall = `{"kind":"tool_calls","calls":[{"name":"terminal","arguments":` +
	`{"input":"id","cwd":"/work","detach":false,"timeout":0,"message":"check the current user"}}]}`
