# Claude Code CLI provider adapter

Status: a specified, mock-tested adapter (`pkg/providers/claudecli`), its HTTP client (`HTTPRunner`) and the worker that runs the CLI ([claude_cli_worker.md](claude_cli_worker.md)). The adapter is registered as provider type `claude_cli` (`providerRegistry`, `NewFromConfig` in `door.go`). It is enabled only when `CLAUDE_CLI_WORKER_URL` and `CLAUDE_CLI_WORKER_TOKEN` are set, and a flow uses it only when that flow selects it. The companion documents are [claude_cli_tool_calls.md](claude_cli_tool_calls.md) and [scope_approval_policy.md](scope_approval_policy.md).

## Purpose and boundaries

The adapter lets a locally authenticated Claude Code CLI serve as the language model behind PentAGI agents while PentAGI keeps everything else: the agent loop in `pkg/providers/performer.go`, tool execution in `pkg/tools`, chain persistence in `msgchains`, summarization, and every guard around tool calls.

The CLI is a completion engine and nothing more. It receives a rendered conversation and returns either text or a *proposal* of tool calls. It never executes a PentAGI tool, never keeps a session, and must expose none of its own tools.

It is an original implementation written against this specification. It reuses no code, identifiers, regular expressions or prompt text from other projects.

## Architecture

```
performAgentChain ──CallWithTools──► claudecli.Provider ──Runner.Run(Request)──► worker ──► claude -p (stateless, no tools)
        ▲                                   │  render chain → system prompt + transcript
        │                                   │  build decision schema from the offered tools
        │                                   │  verify result evidence, validate decision, mint IDs
        └──── llms.ContentResponse ◄────────┘
```

`Provider` implements `provider.Provider` in full. Every `Call*` method goes through `provider.WrapGenerateContent` (`Call` wraps its prompt as a one-message chain), so Langfuse generations, output caps, the context-window check and the 429 backoff behave exactly as for the API doors.

The adapter talks to the CLI only through the `Runner` interface (`runner.go`). `HTTPRunner` implements it over HTTP or a Unix socket (`UnixSocketTransport`) against the worker in `pkg/claudeworker`, which owns process spawning, flags, isolation and credentials. The adapter package contains no `exec` call.

`HTTPRunner` maps the worker's error kinds as follows:

| Worker kind | Adapter error |
|---|---|
| `timeout` | `ErrCallTimeout` |
| `bad_request` | `ErrUnsupportedRequest` (fatal) |
| `unauthorized`, `auth` | `ErrAuth` (fatal) |
| `rate_limited` | throttling backoff |
| anything else | `ErrWorkerUnavailable` |

The bearer token never appears in an error.

## Construction

`New(Options)` takes:

| Field | Meaning |
|---|---|
| `Name` | Provider name reported to PentAGI |
| `Config` | `*pconfig.ProviderConfig`; per-agent model selection comes from it |
| `Models` | Catalogue used by `ModelLimitsFor` (context window, max output) |
| `DefaultModel` | Model when an agent section names none |
| `ToolCallIDTemplate` | Pattern for minted IDs; must be a valid `templates` pattern |
| `Runner` | Required; must be safe for concurrent use and honour `ctx` |
| `CallTimeout` | Per-call bound; zero means 10 minutes |

The package ships no `config.yml` or `models.yml`. The caller injects the Anthropic door's catalogue, because the CLI serves the same model family. Shipping a second `models.yml` would also trip the catalogue guard tests in `pkg/providers`.

## Request modes

| Caller | Mode | Output schema sent | Reply read from |
|---|---|---|---|
| `CallWithTools` / `CallWithExtraOptions` with tools | decision | decision schema ([tool-call spec](claude_cli_tool_calls.md)) | `structured_output` |
| any call with `llms.WithJSONMode()` and no tools | JSON | `{"type":"object"}` | `structured_output`, must be an object |
| `Call`, `CallEx`, no tools | text | none | `result` text, trimmed, must be non-empty |

These are rejected before the CLI is called, with `ErrUnsupportedRequest`:
- tools together with JSON mode;
- a caller-supplied `WithStructuredOutput` schema;
- image or binary parts;
- unknown roles;
- duplicate tool names;
- a tool schema that does not compile as JSON Schema draft-07;
- any conversation content that contains the transcript marker.

Sampling and reasoning options are ignored: the CLI exposes no flags for them.

## Worker protocol (version 1)

`Request` (adapter → worker):

| Field | Type | Notes |
|---|---|---|
| `version` | int | `1` |
| `request_id` | string | UUID per call, for correlation only |
| `agent` | string | PentAGI agent type, for logging |
| `model` | string | Resolved per agent from the provider config |
| `system_prompt` | string | Preamble + mode instructions + the chain's system messages |
| `transcript` | string | Must reach the CLI on stdin, never in argv |
| `output_schema` | JSON | Present in decision and JSON modes |
| `timeout_ms` | int | The adapter's own bound; the worker must not exceed it |

`Result` (worker → adapter) mirrors the CLI's final result message, plus evidence the worker collects from the stream-json events:

| Field | Source |
|---|---|
| `subtype`, `is_error`, `result`, `structured_output`, `errors`, `stop_reason`, `num_turns`, `usage{input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens}`, `total_cost_usd` | CLI result message |
| `permission_denials` | Count from the result message |
| `init_tools` | Tool list of the `system/init` event |
| `tool_uses` | Name of every `tool_use` block seen in the stream |
| `cli_version` | `claude --version` of the worker's pinned binary |

A failure before a result exists is a `WorkerError{Kind, Message}`, where `Kind` is one of `unavailable`, `overloaded`, `rate_limited`, `auth` or `internal`.

## Verification (fail closed)

Before the reply is read, `verify` rejects the result with `ErrNativeToolActivity` (fatal) if any of these holds:
- `permission_denials > 0`;
- any `init_tools` entry other than `StructuredOutput`;
- any `tool_uses` entry other than `StructuredOutput`.

`StructuredOutput` is accepted only when a schema was sent. In text mode, even that tool is a violation.

The check runs on error results too, so evidence of tool activity always wins over an ordinary failure.

Whether the CLI really lists `StructuredOutput` in `init_tools` once all built-in tools are removed is an open question for Tier B. The name is a constant in one place (`structuredOutputTool`).

## Error contract

| Condition | Error | Fatal (`errors.Is(err, ErrFatal)`) | Agent loop effect |
|---|---|---|---|
| Caller cancelled | wraps `context.Canceled` | — | Loop unwinds as for any provider |
| Result arrived after cancellation | wraps `context.Canceled`; result discarded | — | Same |
| Own deadline (`CallTimeout`) | `ErrCallTimeout`, deliberately **not** wrapping `context.DeadlineExceeded` | no | Retry, then reflector; failover may switch |
| Worker unreachable, crashed, overloaded, no result | `ErrWorkerUnavailable` | no | Retry, then reflector |
| `rate_limited` | `WorkerError` whose text contains "rate limit exceeded" | no | The shared wrapper backs off and retries |
| `auth` | `ErrAuth` | yes | — |
| Native tool activity | `ErrNativeToolActivity` | yes | — |
| `error_max_budget_usd` | `ErrBudgetExceeded` | yes | — |
| `error_max_structured_output_retries`, invalid or missing decision, empty text | `ErrInvalidDecision` | no | Retry, then reflector |
| Other `error_*` subtypes | `ErrCLIExecution` | no | Retry, then reflector |
| Unsupported request | `ErrUnsupportedRequest` | yes | — |

`ErrFatal` carries a `Deterministic() bool` method, which `providers.isDeterministicError` honours. A fatal adapter error therefore stops the agent loop's retries at once, without `pkg/providers` importing this package. No API door error implements the method, so API behaviour is unchanged.

## Usage accounting

`GenerationInfo` carries `PromptTokens`, `CompletionTokens`, `CacheReadInputTokens` and `CacheCreationInputTokens`, so `pconfig.NewCallUsage` and the `msgchains.usage_*` columns work unchanged. Cost comes from the catalogue price, as for API doors.

The CLI's own estimate is kept as `ClaudeCLICostEstimateUSD`, beside `ClaudeCLIStopReason`, `ClaudeCLINumTurns` and `ClaudeCLIVersion`. On a subscription, cost figures are notional.

## Sessions and isolation

Every call is stateless: the whole chain is rendered every time, and the adapter never sends a session ID. Per-agent and per-task isolation therefore follows from PentAGI's own `msgchains`. A resumed CLI session would diverge from the chain PentAGI persists, because summarization, compaction and ID normalization all rewrite it.

## Streaming

Partial streaming is not provided. A structured decision cannot be shown before it validates. The agent loop already sends the final content to the assistant UI after the call returns.

## Concurrency

`Provider` holds no mutable state. Built-in providers are shared between flows and assistants, so the `Runner` must be safe for concurrent use. A test runs 16 parallel calls under `-race`.

## Not built yet

The following are not built yet:
- the web UI label, icon and settings form (Phase D), and choosing API or CLI per agent within one flow;
- wiring of the Scope & Approval Gateway;
- the installed deployment, which exists only as review-only files in `examples/claude-worker`;
- any run against the real CLI.
