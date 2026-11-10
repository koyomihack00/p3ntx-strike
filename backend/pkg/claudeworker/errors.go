package claudeworker

import "pentagi/pkg/claudeworker/protocol"

// Error is a worker failure before or instead of a CLI result. Message is composed
// by the worker and never carries CLI output, prompt content or credentials.
type Error struct {
	Kind    protocol.ErrorKind
	Message string
}

func (e *Error) Error() string { return string(e.Kind) + ": " + e.Message }
