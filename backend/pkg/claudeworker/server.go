package claudeworker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"pentagi/pkg/claudeworker/protocol"

	"github.com/sirupsen/logrus"
)

const (
	maxRequestBytes = 16 << 20
	minTokenLength  = 32
	maxLoggedID     = 64
)

// Runner is satisfied by Executor; the server depends on the interface so its
// HTTP behaviour is testable without spawning processes.
type Runner interface {
	Run(ctx context.Context, req protocol.Request) (*protocol.Result, error)
}

type Server struct {
	runner Runner
	token  []byte
	log    logrus.FieldLogger
	mux    *http.ServeMux
}

func NewServer(runner Runner, token string, log logrus.FieldLogger) (*Server, error) {
	if runner == nil {
		return nil, errors.New("claude worker server needs a runner")
	}
	if len(token) < minTokenLength {
		return nil, errors.New("claude worker token is too short")
	}
	if log == nil {
		log = logrus.StandardLogger()
	}
	s := &Server{runner: runner, token: []byte(token), log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("POST "+protocol.RunPath, s.handleRun)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), s.token) == 1
}

var statusByKind = map[protocol.ErrorKind]int{
	protocol.KindBadRequest:   http.StatusBadRequest,
	protocol.KindUnauthorized: http.StatusUnauthorized,
	protocol.KindRateLimited:  http.StatusTooManyRequests,
	protocol.KindOverloaded:   http.StatusServiceUnavailable,
	protocol.KindUnavailable:  http.StatusServiceUnavailable,
	protocol.KindTimeout:      http.StatusGatewayTimeout,
	protocol.KindAuth:         http.StatusBadGateway,
	protocol.KindInternal:     http.StatusInternalServerError,
}

func writeError(w http.ResponseWriter, kind protocol.ErrorKind, msg string) {
	status, ok := statusByKind[kind]
	if !ok {
		kind, status = protocol.KindInternal, http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(protocol.ErrorBody{Kind: kind, Message: msg})
}

func loggable(id string) string {
	if !utf8.ValidString(id) {
		return "invalid"
	}
	if len(id) > maxLoggedID {
		return id[:maxLoggedID]
	}
	return id
}

// handleRun logs one line per request with sizes and outcome only: the transcript,
// system prompt, schema, CLI output and credentials never reach the log.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	fields := logrus.Fields{"remote": r.RemoteAddr}
	outcome := "ok"
	defer func() {
		fields["outcome"] = outcome
		fields["duration_ms"] = time.Since(start).Milliseconds()
		s.log.WithFields(fields).Info("claude worker request")
	}()

	if !s.authorized(r) {
		outcome = string(protocol.KindUnauthorized)
		writeError(w, protocol.KindUnauthorized, "missing or invalid bearer token")
		return
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	var req protocol.Request
	if err := dec.Decode(&req); err != nil {
		outcome = string(protocol.KindBadRequest)
		writeError(w, protocol.KindBadRequest, "request body is not a valid run request")
		return
	}
	fields["request_id"] = loggable(req.RequestID)
	fields["agent"] = loggable(req.Agent)
	fields["model"] = loggable(req.Model)
	fields["transcript_bytes"] = len(req.Transcript)
	fields["system_prompt_bytes"] = len(req.SystemPrompt)
	fields["schema_bytes"] = len(req.OutputSchema)

	res, err := s.runner.Run(r.Context(), req)
	if err != nil {
		var werr *Error
		switch {
		case errors.As(err, &werr):
			outcome = string(werr.Kind)
			writeError(w, werr.Kind, werr.Message)
		case r.Context().Err() != nil:
			outcome = "client_gone"
		default:
			outcome = string(protocol.KindInternal)
			writeError(w, protocol.KindInternal, "worker failure")
		}
		return
	}

	fields["cli_subtype"] = loggable(res.Subtype)
	fields["init_tools"] = len(res.InitTools)
	fields["tool_uses"] = len(res.ToolUses)
	fields["permission_denials"] = res.PermissionDenials
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
