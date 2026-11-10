package claudecli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"pentagi/pkg/cast"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/templates"

	"github.com/vxcontrol/langchaingo/llms"
)

func TestClaudeCLI_CallWithTools_ReturnsToolCallWithMintedID(t *testing.T) {
	runner := &fakeRunner{reply: replyWith(structured(terminalCall))}
	p := newTestProvider(t, runner)

	resp, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
		pentagiTools(t, "terminal", "hack_result"), nil)
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}

	choice := resp.Choices[0]
	if len(choice.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(choice.ToolCalls))
	}
	call := choice.ToolCalls[0]
	if call.FunctionCall.Name != "terminal" {
		t.Errorf("name = %q, want terminal", call.FunctionCall.Name)
	}
	wantArgs := `{"input":"id","cwd":"/work","detach":false,"timeout":0,"message":"check the current user"}`
	if call.FunctionCall.Arguments != wantArgs {
		t.Errorf("arguments = %s, want %s", call.FunctionCall.Arguments, wantArgs)
	}
	if err := templates.ValidatePattern("toolu_{r:24:b}", []templates.PatternSample{{Value: call.ID}}); err != nil {
		t.Errorf("ID %q does not follow the template: %v", call.ID, err)
	}
	if choice.StopReason != "tool_use" || choice.Content != "" {
		t.Errorf("stop=%q content=%q, want tool_use and no content", choice.StopReason, choice.Content)
	}

	chain := append(taskChain(),
		llms.MessageContent{Role: llms.ChatMessageTypeAI, Parts: []llms.ContentPart{call}},
		llms.MessageContent{Role: llms.ChatMessageTypeTool, Parts: []llms.ContentPart{
			llms.ToolCallResponse{ToolCallID: call.ID, Name: "terminal", Content: "uid=0(root)"},
		}},
	)
	if _, err := cast.NewChainAST(chain, false); err != nil {
		t.Errorf("chain with the returned call is not a valid PentAGI chain: %v", err)
	}

	req := runner.lastRequest(t)
	if req.Agent != "pentester" || req.Model != "claude-sonnet-5" || len(req.OutputSchema) == 0 {
		t.Errorf("request agent=%q model=%q schema=%d bytes", req.Agent, req.Model, len(req.OutputSchema))
	}
}

func TestClaudeCLI_CallWithTools_ReportsUsageInPentAGIKeys(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{reply: replyWith(structured(terminalCall))})

	resp, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
		pentagiTools(t, "terminal"), nil)
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}

	usage := p.GetUsage(resp.Choices[0].GenerationInfo)
	want := pconfig.CallUsage{Input: 1200, Output: 80, CacheRead: 300, CacheWrite: 40}
	if usage.Input != want.Input || usage.Output != want.Output ||
		usage.CacheRead != want.CacheRead || usage.CacheWrite != want.CacheWrite {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
	if got := resp.Choices[0].GenerationInfo["ClaudeCLICostEstimateUSD"]; got != 0.0123 {
		t.Errorf("cost estimate = %v, want 0.0123", got)
	}
}

func TestClaudeCLI_CallWithTools_TextDecisionCarriesNoToolCalls(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{reply: replyWith(structured(`{"kind":"text","content":"The shell runs as root."}`))})

	resp, err := p.CallWithTools(context.Background(), pconfig.OptionsTypeAssistant, taskChain(),
		pentagiTools(t, "terminal"), nil)
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	choice := resp.Choices[0]
	if choice.Content != "The shell runs as root." || len(choice.ToolCalls) != 0 || choice.StopReason != "end_turn" {
		t.Errorf("content=%q calls=%d stop=%q", choice.Content, len(choice.ToolCalls), choice.StopReason)
	}
}

func TestClaudeCLI_GetToolCallIDTemplate_DoesNotCallTheModel(t *testing.T) {
	runner := &fakeRunner{reply: blockUntilDone}
	p := newTestProvider(t, runner)

	got, err := p.GetToolCallIDTemplate(context.Background(), nil)
	if err != nil || got != "toolu_{r:24:b}" {
		t.Errorf("template = %q, %v", got, err)
	}
	if runner.calls() != 0 {
		t.Errorf("runner called %d times, want 0", runner.calls())
	}
}

func TestClaudeCLI_Verify_RejectsNativeToolActivity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Result)
	}{
		{"exposed bash", func(r *Result) { r.InitTools = append(r.InitTools, "Bash") }},
		{"used read", func(r *Result) { r.ToolUses = append(r.ToolUses, "Read") }},
		{"used an mcp tool", func(r *Result) { r.ToolUses = []string{"mcp__pentagi__terminal"} }},
		{"permission denial", func(r *Result) { r.PermissionDenials = 1 }},
		{"denial on an error result", func(r *Result) { r.IsError, r.Subtype, r.PermissionDenials = true, "error_during_execution", 2 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := structured(terminalCall)
			tc.mutate(res)
			p := newTestProvider(t, &fakeRunner{reply: replyWith(res)})

			resp, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
				pentagiTools(t, "terminal"), nil)
			if !errors.Is(err, ErrNativeToolActivity) || !errors.Is(err, ErrFatal) {
				t.Fatalf("err = %v, want ErrNativeToolActivity", err)
			}
			if resp != nil {
				t.Errorf("a response escaped: %+v", resp)
			}
		})
	}
}

func TestClaudeCLI_Verify_StructuredOutputToolForbiddenWithoutSchema(t *testing.T) {
	res := &Result{Subtype: "success", Text: "A title", InitTools: []string{"StructuredOutput"}}
	p := newTestProvider(t, &fakeRunner{reply: replyWith(res)})

	_, err := p.Call(context.Background(), pconfig.OptionsTypeSimple, "Name this task")
	if !errors.Is(err, ErrNativeToolActivity) {
		t.Errorf("err = %v, want ErrNativeToolActivity", err)
	}
}

func TestClaudeCLI_Verify_MapsErrorSubtypes(t *testing.T) {
	cases := []struct {
		subtype   string
		want      error
		wantFatal bool
	}{
		{"error_max_structured_output_retries", ErrInvalidDecision, false},
		{"error_max_budget_usd", ErrBudgetExceeded, true},
		{"error_during_execution", ErrCLIExecution, false},
		{"error_max_turns", ErrCLIExecution, false},
	}
	for _, tc := range cases {
		t.Run(tc.subtype, func(t *testing.T) {
			res := structured(terminalCall)
			res.IsError, res.Subtype, res.Errors = true, tc.subtype, []string{"detail"}
			p := newTestProvider(t, &fakeRunner{reply: replyWith(res)})

			_, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
				pentagiTools(t, "terminal"), nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if errors.Is(err, ErrFatal) != tc.wantFatal {
				t.Errorf("fatal = %v, want %v", errors.Is(err, ErrFatal), tc.wantFatal)
			}
		})
	}
}

func TestClaudeCLI_Verify_ClassifiesCLIFailures(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		want      error
		wantFatal bool
		throttled bool
	}{
		{"expired login", "OAuth token has expired. Please obtain a new token or refresh your existing token.", ErrAuth, true, false},
		{"401 from the API", `API Error: 401 {"type":"error","error":{"type":"authentication_error"}}`, ErrAuth, true, false},
		{"not logged in", "Not logged in · Please run /login", ErrAuth, true, false},
		{"subscription usage limit", "Claude AI usage limit reached|1762790400", ErrUsageLimit, true, false},
		{"rate limited", "API Error: 429 rate_limit_error", ErrCLIExecution, false, true},
		{"overloaded", "API Error: 529 Overloaded", ErrCLIExecution, false, true},
		{"schema rejected", "API Error: 400 tools.1.custom.input_schema.type: Field required", ErrCLIExecution, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &Result{Subtype: "success", IsError: true, Text: tc.text}
			p := newTestProvider(t, &fakeRunner{reply: func(ctx context.Context, call int, _ Request) (*Result, error) {
				if call > 1 {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return res, nil
			}})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, err := p.Call(ctx, pconfig.OptionsTypeSimple, "hello")
			if !tc.throttled && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if errors.Is(err, ErrFatal) != tc.wantFatal {
				t.Errorf("fatal = %v, want %v (%v)", errors.Is(err, ErrFatal), tc.wantFatal, err)
			}
			if tc.throttled && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("throttling did not enter the shared backoff: %v", err)
			}
		})
	}
}

func TestClaudeCLI_Run_MapsWorkerFailures(t *testing.T) {
	cases := []struct {
		name      string
		reply     func(context.Context, int, Request) (*Result, error)
		want      error
		wantFatal bool
	}{
		{"transport", func(context.Context, int, Request) (*Result, error) {
			return nil, errors.New("dial unix /run/claude-worker.sock: connection refused")
		}, ErrWorkerUnavailable, false},
		{"worker crash", func(context.Context, int, Request) (*Result, error) {
			return nil, &WorkerError{Kind: WorkerInternal, Message: "cli exited with status 137"}
		}, ErrWorkerUnavailable, false},
		{"auth", func(context.Context, int, Request) (*Result, error) {
			return nil, &WorkerError{Kind: WorkerAuth, Message: "not logged in"}
		}, ErrAuth, true},
		{"no result", func(context.Context, int, Request) (*Result, error) { return nil, nil }, ErrWorkerUnavailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{reply: tc.reply}
			p := newTestProvider(t, runner)

			_, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
				pentagiTools(t, "terminal"), nil)
			if !errors.Is(err, tc.want) || errors.Is(err, ErrFatal) != tc.wantFatal {
				t.Fatalf("err = %v, want %v (fatal %v)", err, tc.want, tc.wantFatal)
			}
			if runner.calls() != 1 {
				t.Errorf("runner called %d times, want 1", runner.calls())
			}
		})
	}
}

func TestClaudeCLI_Run_KeepsClassifiedRunnerErrors(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		want      error
		wantFatal bool
	}{
		{"worker deadline", fmt.Errorf("%w: worker deadline", ErrCallTimeout), ErrCallTimeout, false},
		{"worker rejected the request", fmt.Errorf("%w: bad", ErrUnsupportedRequest), ErrUnsupportedRequest, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProvider(t, &fakeRunner{reply: func(context.Context, int, Request) (*Result, error) { return nil, tc.err }})

			_, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
				pentagiTools(t, "terminal"), nil)
			if !errors.Is(err, tc.want) || errors.Is(err, ErrFatal) != tc.wantFatal {
				t.Fatalf("err = %v, want %v (fatal %v)", err, tc.want, tc.wantFatal)
			}
			if errors.Is(err, ErrWorkerUnavailable) {
				t.Errorf("a classified error was relabelled as worker unavailable: %v", err)
			}
		})
	}
}

func TestClaudeCLI_Run_RateLimitIsRetriedByTheSharedWrapper(t *testing.T) {
	runner := &fakeRunner{reply: func(_ context.Context, call int, _ Request) (*Result, error) {
		if call == 1 {
			return nil, &WorkerError{Kind: WorkerRateLimited, Message: "usage limit reached"}
		}
		return structured(terminalCall), nil
	}}
	p := newTestProvider(t, runner)

	resp, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
		pentagiTools(t, "terminal"), nil)
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	if runner.calls() != 2 || len(resp.Choices[0].ToolCalls) != 1 {
		t.Errorf("calls=%d toolCalls=%d, want 2 and 1", runner.calls(), len(resp.Choices[0].ToolCalls))
	}
}

func TestClaudeCLI_Run_TimeoutIsRetryableAndNotACancellation(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{reply: blockUntilDone}, func(o *Options) { o.CallTimeout = 50 * time.Millisecond })

	done := make(chan error, 1)
	go func() {
		_, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
			pentagiTools(t, "terminal"), nil)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCallTimeout) {
			t.Fatalf("err = %v, want ErrCallTimeout", err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrFatal) {
			t.Errorf("timeout must read as a retryable model failure, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("call did not return after its 50ms timeout")
	}
}

func TestClaudeCLI_Run_CancellationPropagatesAndStopsTheRunner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	runner := &fakeRunner{reply: func(ctx context.Context, _ int, _ Request) (*Result, error) {
		cancel()
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}}
	p := newTestProvider(t, runner)

	_, err := p.CallWithTools(ctx, pconfig.OptionsTypePentester, taskChain(), pentagiTools(t, "terminal"), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never saw the cancellation")
	}
}

func TestClaudeCLI_Run_ResultAfterCancellationIsDiscarded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newTestProvider(t, ignoresCtxRunner(func() *Result {
		cancel()
		return structured(terminalCall)
	}))

	resp, err := p.CallWithTools(ctx, pconfig.OptionsTypePentester, taskChain(), pentagiTools(t, "terminal"), nil)
	if !errors.Is(err, context.Canceled) || resp != nil {
		t.Errorf("resp=%v err=%v, want no response and context.Canceled", resp, err)
	}
}

// ignoresCtxRunner models a worker that answers even though the caller cancelled.
type ignoresCtxRunner func() *Result

func (r ignoresCtxRunner) Run(context.Context, Request) (*Result, error) {
	return r(), nil
}

func TestClaudeCLI_CallWithTools_IsSafeForConcurrentUse(t *testing.T) {
	runner := &fakeRunner{reply: replyWith(structured(terminalCall))}
	p := newTestProvider(t, runner)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
				pentagiTools(t, "terminal"), nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent call: %v", err)
		}
	}
	if runner.calls() != 16 {
		t.Errorf("runner called %d times, want 16", runner.calls())
	}
}

func TestClaudeCLI_Call_PlainTextHasNoSchema(t *testing.T) {
	runner := &fakeRunner{reply: replyWith(&Result{Subtype: "success", Text: "  Enumerate the web server  \n"})}
	p := newTestProvider(t, runner)

	got, err := p.Call(context.Background(), pconfig.OptionsTypeSimple, "Name this task")
	if err != nil || got != "Enumerate the web server" {
		t.Fatalf("Call = %q, %v", got, err)
	}
	if req := runner.lastRequest(t); len(req.OutputSchema) != 0 || req.Model != "claude-haiku-4-5" {
		t.Errorf("schema=%s model=%q, want none and claude-haiku-4-5", req.OutputSchema, req.Model)
	}
}

func TestClaudeCLI_Call_EmptyCompletionFails(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{reply: replyWith(&Result{Subtype: "success", Text: " \n"})})

	if _, err := p.Call(context.Background(), pconfig.OptionsTypeSimple, "Name this task"); !errors.Is(err, ErrInvalidDecision) {
		t.Errorf("err = %v, want ErrInvalidDecision", err)
	}
}

func TestClaudeCLI_CallWithExtraOptions_JSONModeReturnsTheObject(t *testing.T) {
	runner := &fakeRunner{reply: replyWith(structured(`{ "input": "id", "cwd": "/work" }`))}
	p := newTestProvider(t, runner)

	resp, err := p.CallWithExtraOptions(context.Background(), pconfig.OptionsTypeSimpleJSON, taskChain(), nil, nil,
		llms.WithJSONMode())
	if err != nil {
		t.Fatalf("CallWithExtraOptions: %v", err)
	}
	if got := resp.Choices[0].Content; got != `{"input":"id","cwd":"/work"}` {
		t.Errorf("content = %s", got)
	}
	if req := runner.lastRequest(t); string(req.OutputSchema) != `{"type":"object"}` {
		t.Errorf("schema = %s", req.OutputSchema)
	}
}

func TestClaudeCLI_CallWithExtraOptions_JSONModeRejectsNonObjects(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{reply: replyWith(structured(`[1,2]`))})

	_, err := p.CallWithExtraOptions(context.Background(), pconfig.OptionsTypeSimpleJSON, taskChain(), nil, nil,
		llms.WithJSONMode())
	if !errors.Is(err, ErrInvalidDecision) {
		t.Errorf("err = %v, want ErrInvalidDecision", err)
	}
}

func TestClaudeCLI_Generate_RejectsUnsupportedRequestsBeforeCallingTheCLI(t *testing.T) {
	cases := []struct {
		name  string
		chain []llms.MessageContent
		tools bool
		extra []llms.CallOption
	}{
		{"image part", []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{
			llms.ImageURLContent{URL: "https://example.invalid/x.png"}}}}, false, nil},
		{"tools with JSON mode", taskChain(), true, []llms.CallOption{llms.WithJSONMode()}},
		{"caller structured output", taskChain(), false, []llms.CallOption{
			llms.WithStructuredOutput(llms.StructuredOutputConfig{Name: "x", Schema: []byte(`{"type":"object"}`)})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{reply: replyWith(structured(terminalCall))}
			p := newTestProvider(t, runner)
			var tl []llms.Tool
			if tc.tools {
				tl = pentagiTools(t, "terminal")
			}

			_, err := p.CallWithExtraOptions(context.Background(), pconfig.OptionsTypeSimple, tc.chain, tl, nil, tc.extra...)
			if !errors.Is(err, ErrUnsupportedRequest) {
				t.Fatalf("err = %v, want ErrUnsupportedRequest", err)
			}
			if runner.calls() != 0 {
				t.Errorf("runner called %d times, want 0", runner.calls())
			}
		})
	}
}

func TestClaudeCLI_New_RejectsIncompleteOptions(t *testing.T) {
	runner := &fakeRunner{reply: blockUntilDone}
	base := newTestProvider(t, runner)
	valid := Options{Name: "x", Config: base.config, DefaultModel: "m", ToolCallIDTemplate: "toolu_{r:24:b}", Runner: runner}

	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{"no runner", func(o *Options) { o.Runner = nil }},
		{"no config", func(o *Options) { o.Config = nil }},
		{"no model", func(o *Options) { o.DefaultModel = "" }},
		{"no name", func(o *Options) { o.Name = "" }},
		{"no template", func(o *Options) { o.ToolCallIDTemplate = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := valid
			tc.mutate(&o)
			if _, err := New(o); err == nil {
				t.Error("New accepted incomplete options")
			}
		})
	}
	if _, err := New(valid); err != nil {
		t.Errorf("New rejected valid options: %v", err)
	}
}
