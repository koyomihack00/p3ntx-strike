// Package claudecli is a provider.Provider whose completions come from Claude Code
// in headless mode, reached through a Runner. The CLI only ever proposes: a tool
// call comes back as a schema-validated decision and PentAGI's own executor runs it.
package claudecli

import (
	"context"
	"errors"
	"fmt"

	"pentagi/pkg/claudeworker/protocol"
)

const ProtocolVersion = protocol.Version

type (
	Request = protocol.Request
	Usage   = protocol.Usage
	Result  = protocol.Result
)

// Runner must be safe for concurrent use and must stop work when ctx is done.
type Runner interface {
	Run(ctx context.Context, req Request) (*Result, error)
}

type WorkerErrorKind = protocol.ErrorKind

const (
	WorkerUnavailable = protocol.KindUnavailable
	WorkerOverloaded  = protocol.KindOverloaded
	WorkerRateLimited = protocol.KindRateLimited
	WorkerAuth        = protocol.KindAuth
	WorkerInternal    = protocol.KindInternal
)

// WorkerError is a failure before the CLI produced a result message.
type WorkerError struct {
	Kind    WorkerErrorKind
	Message string
}

// Error keeps the phrase "rate limit exceeded" for WorkerRateLimited: the shared
// provider wrapper recognises throttling by that text and backs off.
func (e *WorkerError) Error() string {
	if e.Kind == WorkerRateLimited {
		return fmt.Sprintf("claude worker: rate limit exceeded: %s", e.Message)
	}
	return fmt.Sprintf("claude worker: %s: %s", e.Kind, e.Message)
}

type fatalError struct{ msg string }

func (e *fatalError) Error() string { return e.msg }

// Deterministic tells the agent loop's retry classification that repeating the
// same request cannot succeed.
func (e *fatalError) Deterministic() bool { return true }

var (
	// ErrFatal marks failures the same request cannot recover from by retrying.
	ErrFatal error = &fatalError{msg: "claude cli: fatal"}

	ErrNativeToolActivity = fmt.Errorf("%w: the CLI session exposed or used a native tool", ErrFatal)
	ErrAuth               = fmt.Errorf("%w: authentication failed", ErrFatal)
	ErrBudgetExceeded     = fmt.Errorf("%w: per-call budget exceeded", ErrFatal)
	ErrUsageLimit         = fmt.Errorf("%w: Claude subscription usage limit reached", ErrFatal)
	ErrUnsupportedRequest = fmt.Errorf("%w: unsupported request", ErrFatal)

	ErrInvalidDecision   = errors.New("claude cli: invalid decision")
	ErrCLIExecution      = errors.New("claude cli: execution failed")
	ErrWorkerUnavailable = errors.New("claude cli: worker unavailable")
	ErrCallTimeout       = errors.New("claude cli: call timed out")
)
