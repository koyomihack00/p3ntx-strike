package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pentagi/pkg/claudeworker/protocol"
)

const runnerBearer = "httprunner-test-bearer-0123456789abcdef"

func workerStub(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestHTTPRunner_Run_PostsTheRequestWithTheBearer(t *testing.T) {
	var got protocol.Request
	ts := workerStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/run" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+runnerBearer {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"subtype":"success","num_turns":2,"init_tools":["StructuredOutput"]}`))
	})
	runner, err := NewHTTPRunner(ts.URL+"/", runnerBearer, ts.Client())
	if err != nil {
		t.Fatal(err)
	}

	res, err := runner.Run(context.Background(), Request{Version: 1, RequestID: "abc", Transcript: "t"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.NumTurns != 2 || res.InitTools[0] != "StructuredOutput" || got.RequestID != "abc" || got.Transcript != "t" {
		t.Errorf("res=%+v sent=%+v", res, got)
	}
}

func TestHTTPRunner_Run_MapsWorkerErrors(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		want      error
		wantKind  WorkerErrorKind
		wantFatal bool
	}{
		{504, `{"kind":"timeout","message":"m"}`, ErrCallTimeout, "", false},
		{400, `{"kind":"bad_request","message":"m"}`, ErrUnsupportedRequest, "", true},
		{401, `{"kind":"unauthorized","message":"m"}`, nil, WorkerAuth, false},
		{502, `{"kind":"auth","message":"m"}`, nil, WorkerAuth, false},
		{503, `{"kind":"overloaded","message":"m"}`, nil, WorkerOverloaded, false},
		{429, `{"kind":"rate_limited","message":"m"}`, nil, WorkerRateLimited, false},
		{500, `{"kind":"internal","message":"m"}`, nil, WorkerInternal, false},
		{500, `<html>proxy error</html>`, nil, WorkerInternal, false},
		{418, `{"kind":"teapot","message":"m"}`, nil, WorkerInternal, false},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			ts := workerStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			runner, _ := NewHTTPRunner(ts.URL, runnerBearer, ts.Client())

			_, err := runner.Run(context.Background(), Request{Version: 1})
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.wantKind != "" {
				var werr *WorkerError
				if !errors.As(err, &werr) || werr.Kind != tc.wantKind {
					t.Fatalf("err = %v, want worker error %s", err, tc.wantKind)
				}
			}
			if errors.Is(err, ErrFatal) != tc.wantFatal {
				t.Errorf("fatal = %v, want %v", errors.Is(err, ErrFatal), tc.wantFatal)
			}
		})
	}
}

func TestHTTPRunner_Run_UnreachableWorkerHidesTheToken(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	runner, _ := NewHTTPRunner(url, runnerBearer, &http.Client{Timeout: 5 * time.Second})

	_, err := runner.Run(context.Background(), Request{Version: 1})
	var werr *WorkerError
	if !errors.As(err, &werr) || werr.Kind != WorkerUnavailable {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if strings.Contains(err.Error(), runnerBearer) {
		t.Error("the bearer token leaked into the error")
	}
}

func TestHTTPRunner_Run_CancellationAbortsTheRequest(t *testing.T) {
	release := make(chan struct{})
	ts := workerStub(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	runner, _ := NewHTTPRunner(ts.URL, runnerBearer, ts.Client())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, Request{Version: 1})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the context's error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept waiting after its context ended")
	}
}

func TestHTTPRunner_UnixSocketTransport(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "w.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"subtype":"success"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	runner, err := NewHTTPRunner("http://claude-worker", runnerBearer, &http.Client{Transport: UnixSocketTransport(sock)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.Run(context.Background(), Request{Version: 1})
	if err != nil || res.Subtype != "success" {
		t.Errorf("res=%v err=%v", res, err)
	}
}

func TestHTTPRunner_NewHTTPRunner_RejectsBadSettings(t *testing.T) {
	client := &http.Client{}
	for _, tc := range []struct{ url, token string }{
		{"ftp://worker", runnerBearer}, {"worker:8765", runnerBearer}, {"http://", runnerBearer}, {"http://worker", ""},
	} {
		if _, err := NewHTTPRunner(tc.url, tc.token, client); err == nil {
			t.Errorf("%q accepted", tc.url)
		}
	}
	if _, err := NewHTTPRunner("http://worker", runnerBearer, nil); err == nil {
		t.Error("nil client accepted")
	}
}
