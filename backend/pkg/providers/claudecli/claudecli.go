package claudecli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/templates"

	"github.com/google/uuid"
	"github.com/vxcontrol/langchaingo/llms"
	"github.com/vxcontrol/langchaingo/llms/streaming"
)

const (
	DefaultCallTimeout = 10 * time.Minute
	// structuredOutputTool is the one tool a session may expose and use: Claude
	// Code implements the JSON-schema reply as a tool of that name.
	structuredOutputTool = "StructuredOutput"
)

type Options struct {
	Name         provider.ProviderName
	Config       *pconfig.ProviderConfig
	Models       pconfig.ModelsConfig
	DefaultModel string
	// ToolCallIDTemplate is the pattern minted IDs follow and the template the
	// provider reports for the flow; it must be a valid templates pattern.
	ToolCallIDTemplate string
	Runner             Runner
	// CallTimeout bounds one CLI call; zero means DefaultCallTimeout.
	CallTimeout time.Duration
}

type Provider struct {
	name         provider.ProviderName
	config       *pconfig.ProviderConfig
	models       pconfig.ModelsConfig
	defaultModel string
	tcIDTemplate string
	runner       Runner
	callTimeout  time.Duration
}

var _ provider.Provider = (*Provider)(nil)

func New(o Options) (*Provider, error) {
	switch {
	case o.Runner == nil:
		return nil, errors.New("claude cli provider needs a runner")
	case o.Config == nil:
		return nil, errors.New("claude cli provider needs a provider config")
	case o.DefaultModel == "":
		return nil, errors.New("claude cli provider needs a default model")
	case o.Name == "":
		return nil, errors.New("claude cli provider needs a name")
	}

	sample := templates.GenerateFromPattern(o.ToolCallIDTemplate, "probe")
	if o.ToolCallIDTemplate == "" || templates.ValidatePattern(o.ToolCallIDTemplate,
		[]templates.PatternSample{{Value: sample, FunctionName: "probe"}}) != nil {
		return nil, fmt.Errorf("invalid tool call ID template %q", o.ToolCallIDTemplate)
	}

	timeout := o.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}

	return &Provider{
		name:         o.Name,
		config:       o.Config,
		models:       o.Models,
		defaultModel: o.DefaultModel,
		tcIDTemplate: o.ToolCallIDTemplate,
		runner:       o.Runner,
		callTimeout:  timeout,
	}, nil
}

func (p *Provider) Type() provider.ProviderType                { return provider.ProviderClaudeCLI }
func (p *Provider) Name() provider.ProviderName                { return p.name }
func (p *Provider) GetRawConfig() []byte                       { return p.config.GetRawConfig() }
func (p *Provider) GetProviderConfig() *pconfig.ProviderConfig { return p.config }
func (p *Provider) GetModels() pconfig.ModelsConfig            { return p.models }

func (p *Provider) GetPriceInfo(opt pconfig.ProviderOptionsType) *pconfig.PriceInfo {
	return p.config.GetPriceInfoForType(opt)
}

func (p *Provider) Model(opt pconfig.ProviderOptionsType) string {
	model := p.defaultModel
	opts := llms.CallOptions{Model: &model}
	for _, option := range p.config.GetOptionsForType(opt) {
		option(&opts)
	}
	return opts.GetModel()
}

func (p *Provider) ModelWithPrefix(opt pconfig.ProviderOptionsType) string {
	return p.Model(opt)
}

func (p *Provider) GetUsage(info map[string]any) pconfig.CallUsage {
	return pconfig.NewCallUsage(info)
}

// GetToolCallIDTemplate never probes the model: the adapter mints every ID itself.
func (p *Provider) GetToolCallIDTemplate(context.Context, templates.Prompter) (string, error) {
	return p.tcIDTemplate, nil
}

func (p *Provider) Call(ctx context.Context, opt pconfig.ProviderOptionsType, prompt string) (string, error) {
	resp, err := provider.WrapGenerateContent(ctx, p, opt, p.generator(opt),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, prompt)},
		p.config.GetOptionsForType(opt)...)
	if err != nil {
		return "", err
	}
	return resp.Choices[0].Content, nil
}

func (p *Provider) CallEx(
	ctx context.Context,
	opt pconfig.ProviderOptionsType,
	chain []llms.MessageContent,
	streamCb streaming.Callback,
) (*llms.ContentResponse, error) {
	return provider.WrapGenerateContent(ctx, p, opt, p.generator(opt), chain,
		append([]llms.CallOption{llms.WithStreamingFunc(streamCb)}, p.config.GetOptionsForType(opt)...)...)
}

func (p *Provider) CallWithTools(
	ctx context.Context,
	opt pconfig.ProviderOptionsType,
	chain []llms.MessageContent,
	tools []llms.Tool,
	streamCb streaming.Callback,
) (*llms.ContentResponse, error) {
	return provider.WrapGenerateContent(ctx, p, opt, p.generator(opt), chain,
		append([]llms.CallOption{llms.WithTools(tools), llms.WithStreamingFunc(streamCb)},
			p.config.GetOptionsForType(opt)...)...)
}

func (p *Provider) CallWithExtraOptions(
	ctx context.Context,
	opt pconfig.ProviderOptionsType,
	chain []llms.MessageContent,
	tools []llms.Tool,
	streamCb streaming.Callback,
	extra ...llms.CallOption,
) (*llms.ContentResponse, error) {
	options := []llms.CallOption{llms.WithStreamingFunc(streamCb)}
	if len(tools) > 0 {
		options = append(options, llms.WithTools(tools))
	}
	options = append(options, p.config.GetOptionsForType(opt)...)
	options = append(options, extra...)
	return provider.WrapGenerateContent(ctx, p, opt, p.generator(opt), chain, options...)
}

func (p *Provider) generator(opt pconfig.ProviderOptionsType) provider.GenerateContentFunc {
	return func(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
		return p.generate(ctx, opt, messages, options...)
	}
}

// generate ignores sampling and reasoning options: the CLI exposes no flags for them.
func (p *Provider) generate(
	ctx context.Context,
	opt pconfig.ProviderOptionsType,
	messages []llms.MessageContent,
	options ...llms.CallOption,
) (*llms.ContentResponse, error) {
	opts := llms.CallOptions{}
	for _, option := range options {
		option(&opts)
	}
	if opts.StructuredOutput != nil {
		return nil, fmt.Errorf("%w: caller-supplied structured output schema", ErrUnsupportedRequest)
	}

	mode := replyText
	var spec *decisionSpec
	switch {
	case len(opts.Tools) > 0 && opts.JSONMode:
		return nil, fmt.Errorf("%w: tools together with JSON mode", ErrUnsupportedRequest)
	case len(opts.Tools) > 0:
		var err error
		if spec, err = buildDecisionSpec(opts.Tools); err != nil {
			return nil, err
		}
		mode = replyDecision
	case opts.JSONMode:
		mode = replyJSON
	}

	catalog := ""
	if spec != nil {
		catalog = spec.catalog
	}
	r, err := renderChain(messages, mode, catalog)
	if err != nil {
		return nil, err
	}

	req := Request{
		Version:      ProtocolVersion,
		RequestID:    uuid.NewString(),
		Agent:        string(opt),
		Model:        opts.GetModel(),
		SystemPrompt: r.system,
		Transcript:   r.transcript,
		TimeoutMS:    p.callTimeout.Milliseconds(),
	}
	if req.Model == "" {
		req.Model = p.Model(opt)
	}
	switch mode {
	case replyDecision:
		req.OutputSchema = spec.schema
	case replyJSON:
		req.OutputSchema = jsonObjectSchema
	}

	res, err := p.run(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := verify(res, mode != replyText); err != nil {
		return nil, err
	}

	choice := &llms.ContentChoice{GenerationInfo: generationInfo(res), StopReason: "end_turn"}
	switch mode {
	case replyDecision:
		content, calls, err := spec.parse(res.StructuredOutput, p.tcIDTemplate)
		if err != nil {
			return nil, err
		}
		choice.Content = content
		if len(calls) > 0 {
			choice.ToolCalls = calls
			choice.FuncCall = calls[0].FunctionCall
			choice.StopReason = "tool_use"
		}
	case replyJSON:
		if choice.Content, err = parseJSONObject(res.StructuredOutput); err != nil {
			return nil, err
		}
	default:
		choice.Content = strings.TrimSpace(res.Text)
		if choice.Content == "" {
			return nil, fmt.Errorf("%w: empty completion", ErrInvalidDecision)
		}
	}

	return &llms.ContentResponse{Choices: []*llms.ContentChoice{choice}}, nil
}

// run keeps two failures apart: the caller cancelling (returned wrapped, so
// errors.Is(err, context.Canceled) holds upstream) and this call's own deadline
// (a retryable model failure that must not look like a cancellation).
func (p *Provider) run(ctx context.Context, req Request) (*Result, error) {
	callCtx, cancel := context.WithTimeout(ctx, p.callTimeout)
	defer cancel()

	res, err := p.runner.Run(callCtx, req)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("claude cli call cancelled: %w", ctxErr)
	}
	if callCtx.Err() != nil {
		return nil, fmt.Errorf("%w after %s", ErrCallTimeout, p.callTimeout)
	}
	if err != nil {
		if errors.Is(err, ErrFatal) || errors.Is(err, ErrCallTimeout) {
			return nil, err
		}
		var werr *WorkerError
		if errors.As(err, &werr) {
			switch werr.Kind {
			case WorkerAuth:
				return nil, fmt.Errorf("%w: %s", ErrAuth, werr.Message)
			case WorkerRateLimited:
				return nil, werr
			}
		}
		return nil, fmt.Errorf("%w: %w", ErrWorkerUnavailable, err)
	}
	if res == nil {
		return nil, fmt.Errorf("%w: runner returned no result", ErrWorkerUnavailable)
	}

	return res, nil
}

// verify fails closed on any sign that the session exposed or used a tool beyond
// the structured-output channel, before the reply is looked at.
func verify(res *Result, structured bool) error {
	allowed := func(name string) bool { return structured && name == structuredOutputTool }

	if res.PermissionDenials > 0 {
		return fmt.Errorf("%w: %d permission denials", ErrNativeToolActivity, res.PermissionDenials)
	}
	if i := slices.IndexFunc(res.InitTools, func(n string) bool { return !allowed(n) }); i >= 0 {
		return fmt.Errorf("%w: session exposed tool %q", ErrNativeToolActivity, res.InitTools[i])
	}
	if i := slices.IndexFunc(res.ToolUses, func(n string) bool { return !allowed(n) }); i >= 0 {
		return fmt.Errorf("%w: session used tool %q", ErrNativeToolActivity, res.ToolUses[i])
	}

	if !res.IsError {
		return nil
	}
	detail := strings.Join(res.Errors, "; ")
	if detail == "" {
		detail = res.Text[:min(len(res.Text), 300)]
	}
	if err := classifyCLIFailure(detail); err != nil {
		return err
	}
	switch res.Subtype {
	case "error_max_structured_output_retries":
		return fmt.Errorf("%w: CLI gave up producing valid structured output: %s", ErrInvalidDecision, detail)
	case "error_max_budget_usd":
		return fmt.Errorf("%w: %s", ErrBudgetExceeded, detail)
	default:
		return fmt.Errorf("%w: %s: %s", ErrCLIExecution, res.Subtype, detail)
	}
}

// classifyCLIFailure recognises the failures that retrying the same call cannot fix
// soon, and transient throttling that the shared wrapper should back off from. The
// CLI reports them as text, e.g. "API Error: 401 …" or "Claude AI usage limit
// reached|<reset>".
func classifyCLIFailure(detail string) error {
	lower := strings.ToLower(detail)
	containsAny := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(lower, w) {
				return true
			}
		}
		return false
	}
	switch {
	case containsAny("usage limit", "out of extra usage", "weekly limit", "limit reached|"):
		return fmt.Errorf("%w: %s", ErrUsageLimit, detail)
	case containsAny("api error: 401", "api error: 403", "authentication_error", "oauth token has expired",
		"not logged in", "please run /login", "invalid api key"):
		return fmt.Errorf("%w: the Claude CLI login is missing or expired, run `claude` and log in again: %s", ErrAuth, detail)
	case containsAny("api error: 429", "rate_limit", "rate limit", "api error: 529", "overloaded"):
		return fmt.Errorf("%w: claude cli: rate limit exceeded: %s", ErrCLIExecution, detail)
	}
	return nil
}

func generationInfo(res *Result) map[string]any {
	return map[string]any{
		"PromptTokens":             res.Usage.InputTokens,
		"CompletionTokens":         res.Usage.OutputTokens,
		"CacheReadInputTokens":     res.Usage.CacheReadInputTokens,
		"CacheCreationInputTokens": res.Usage.CacheCreationInputTokens,
		"ClaudeCLICostEstimateUSD": res.TotalCostUSD,
		"ClaudeCLIStopReason":      res.StopReason,
		"ClaudeCLINumTurns":        res.NumTurns,
		"ClaudeCLIVersion":         res.CLIVersion,
	}
}
