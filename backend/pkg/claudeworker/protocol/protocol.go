// Package protocol is the wire contract between the claudecli provider adapter and
// the Claude CLI worker. It has no dependencies so the worker binary stays small.
package protocol

import "encoding/json"

const Version = 1

const RunPath = "/v1/run"

// Request is one stateless CLI invocation. The worker owns the flags; the request
// carries content only.
type Request struct {
	Version      int    `json:"version"`
	RequestID    string `json:"request_id"`
	Agent        string `json:"agent"`
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"`
	// Transcript is sent on the CLI's stdin, never in argv.
	Transcript string `json:"transcript"`
	// OutputSchema, when set, is passed as the CLI's JSON schema; the reply is then
	// read from Result.StructuredOutput only.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	TimeoutMS    int64           `json:"timeout_ms"`
}

type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Result mirrors the CLI's final result message plus the evidence the worker
// gathered from the event stream about which tools the session saw and used.
type Result struct {
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Text             string          `json:"result,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	Errors           []string        `json:"errors,omitempty"`
	StopReason       string          `json:"stop_reason,omitempty"`
	NumTurns         int             `json:"num_turns"`
	Usage            Usage           `json:"usage"`
	// TotalCostUSD is the CLI's own client-side estimate, not a bill.
	TotalCostUSD      float64  `json:"total_cost_usd"`
	PermissionDenials int      `json:"permission_denials"`
	InitTools         []string `json:"init_tools"`
	ToolUses          []string `json:"tool_uses"`
	CLIVersion        string   `json:"cli_version"`
}

type ErrorKind string

const (
	KindUnavailable  ErrorKind = "unavailable"
	KindOverloaded   ErrorKind = "overloaded"
	KindRateLimited  ErrorKind = "rate_limited"
	KindAuth         ErrorKind = "auth"
	KindInternal     ErrorKind = "internal"
	KindBadRequest   ErrorKind = "bad_request"
	KindTimeout      ErrorKind = "timeout"
	KindUnauthorized ErrorKind = "unauthorized"
)

// ErrorBody is the JSON body of every non-200 worker response. Message never
// carries CLI output, prompt content or credentials.
type ErrorBody struct {
	Kind    ErrorKind `json:"kind"`
	Message string    `json:"message"`
}
