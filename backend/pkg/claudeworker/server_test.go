package claudeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pentagi/pkg/claudeworker/protocol"

	"github.com/sirupsen/logrus"
)

const testBearer = "server-test-bearer-token-0123456789abcdef"

type stubRunner struct {
	calls atomic.Int32
	res   *protocol.Result
	err   error
}

func (s *stubRunner) Run(ctx context.Context, _ protocol.Request) (*protocol.Result, error) {
	s.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.res, s.err
}

func newTestServer(t *testing.T, runner Runner) (*Server, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := logrus.New()
	logger.SetOutput(buf)
	logger.SetFormatter(&logrus.JSONFormatter{})
	srv, err := NewServer(runner, testBearer, logger)
	if err != nil {
		t.Fatal(err)
	}
	return srv, buf
}

func postRun(t *testing.T, srv http.Handler, auth string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, protocol.RunPath, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func validBody(t *testing.T) string {
	t.Helper()
	b, _ := json.Marshal(protocol.Request{
		Version: 1, RequestID: "r-1", Agent: "pentester", Model: "claude-sonnet-5",
		SystemPrompt: "SYSTEM-SECRET-PROMPT", Transcript: "TRANSCRIPT-SECRET", TimeoutMS: 1000,
	})
	return string(b)
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) protocol.ErrorBody {
	t.Helper()
	var body protocol.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestServer_HandleRun_RequiresTheBearerToken(t *testing.T) {
	for _, auth := range []string{"", "Bearer wrong", "Basic " + testBearer, "Bearer " + testBearer + "x"} {
		runner := &stubRunner{res: &protocol.Result{Subtype: "success"}}
		srv, _ := newTestServer(t, runner)

		rec := postRun(t, srv, auth, validBody(t))
		if rec.Code != http.StatusUnauthorized || decodeError(t, rec).Kind != protocol.KindUnauthorized {
			t.Errorf("auth %q: status %d", auth, rec.Code)
		}
		if runner.calls.Load() != 0 {
			t.Errorf("auth %q reached the runner", auth)
		}
	}
}

func TestServer_HandleRun_RejectsMalformedBodies(t *testing.T) {
	for _, body := range []string{`{`, `{"version":1,"unexpected":true}`, `[]`} {
		runner := &stubRunner{}
		srv, _ := newTestServer(t, runner)

		rec := postRun(t, srv, "Bearer "+testBearer, body)
		if rec.Code != http.StatusBadRequest || runner.calls.Load() != 0 {
			t.Errorf("body %q: status %d, runner calls %d", body, rec.Code, runner.calls.Load())
		}
	}
}

func TestServer_HandleRun_ReturnsTheResult(t *testing.T) {
	runner := &stubRunner{res: &protocol.Result{Subtype: "success", NumTurns: 2, InitTools: []string{"StructuredOutput"}}}
	srv, _ := newTestServer(t, runner)

	rec := postRun(t, srv, "Bearer "+testBearer, validBody(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var res protocol.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.NumTurns != 2 || res.InitTools[0] != "StructuredOutput" {
		t.Errorf("result %+v, %v", res, err)
	}
}

func TestServer_HandleRun_MapsWorkerErrorsToStatus(t *testing.T) {
	cases := []struct {
		err    error
		status int
		kind   protocol.ErrorKind
	}{
		{&Error{Kind: protocol.KindBadRequest, Message: "m"}, http.StatusBadRequest, protocol.KindBadRequest},
		{&Error{Kind: protocol.KindOverloaded, Message: "m"}, http.StatusServiceUnavailable, protocol.KindOverloaded},
		{&Error{Kind: protocol.KindTimeout, Message: "m"}, http.StatusGatewayTimeout, protocol.KindTimeout},
		{&Error{Kind: protocol.KindAuth, Message: "m"}, http.StatusBadGateway, protocol.KindAuth},
		{&Error{Kind: protocol.KindInternal, Message: "m"}, http.StatusInternalServerError, protocol.KindInternal},
		{&Error{Kind: "invented", Message: "m"}, http.StatusInternalServerError, protocol.KindInternal},
		{errors.New("raw failure carrying TRANSCRIPT-SECRET"), http.StatusInternalServerError, protocol.KindInternal},
	}
	for _, tc := range cases {
		srv, _ := newTestServer(t, &stubRunner{err: tc.err})
		rec := postRun(t, srv, "Bearer "+testBearer, validBody(t))
		body := decodeError(t, rec)
		if rec.Code != tc.status || body.Kind != tc.kind {
			t.Errorf("%v: status %d kind %s, want %d %s", tc.err, rec.Code, body.Kind, tc.status, tc.kind)
		}
		if strings.Contains(body.Message, "TRANSCRIPT-SECRET") {
			t.Error("an unclassified error's text reached the client")
		}
	}
}

func TestServer_HandleRun_LogsMetadataOnly(t *testing.T) {
	srv, logs := newTestServer(t, &stubRunner{res: &protocol.Result{Subtype: "success", ToolUses: []string{"StructuredOutput"}}})

	postRun(t, srv, "Bearer "+testBearer, validBody(t))
	line := logs.String()
	for _, want := range []string{`"request_id":"r-1"`, `"agent":"pentester"`, `"transcript_bytes":17`, `"outcome":"ok"`, `"tool_uses":1`} {
		if !strings.Contains(line, want) {
			t.Errorf("log lacks %s: %s", want, line)
		}
	}
	for _, secret := range []string{"TRANSCRIPT-SECRET", "SYSTEM-SECRET-PROMPT", testBearer} {
		if strings.Contains(line, secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

func TestServer_NewServer_RejectsShortToken(t *testing.T) {
	if _, err := NewServer(&stubRunner{}, "short", nil); err == nil {
		t.Error("a short token was accepted")
	}
}

func TestServer_Healthz(t *testing.T) {
	srv, _ := newTestServer(t, &stubRunner{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status %d", rec.Code)
	}
}
