package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"pentagi/pkg/claudeworker/protocol"
)

const (
	maxWorkerResponseBytes = 16 << 20
	maxWorkerMessageBytes  = 256
)

// HTTPRunner is a Runner that reaches the Claude CLI worker over HTTP. The bearer
// token never appears in an error it returns.
type HTTPRunner struct {
	endpoint string
	token    string
	client   *http.Client
}

func NewHTTPRunner(baseURL, token string, client *http.Client) (*HTTPRunner, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("claude worker URL must be an absolute http(s) URL")
	}
	if token == "" {
		return nil, errors.New("claude worker token is empty")
	}
	if client == nil {
		return nil, errors.New("claude worker HTTP client is nil")
	}
	return &HTTPRunner{
		endpoint: strings.TrimSuffix(u.String(), "/") + protocol.RunPath,
		token:    token,
		client:   client,
	}, nil
}

// UnixSocketTransport dials the worker's Unix socket whatever host the URL names.
func UnixSocketTransport(path string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
}

func (r *HTTPRunner) Run(ctx context.Context, req Request) (*Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: encode worker request: %v", ErrUnsupportedRequest, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build worker request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+r.token)

	resp, err := r.client.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &WorkerError{Kind: WorkerUnavailable, Message: "Claude CLI worker is not reachable; is cmd/claudeworker running?"}
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxWorkerResponseBytes)

	if resp.StatusCode == http.StatusOK {
		var res Result
		if err := json.NewDecoder(limited).Decode(&res); err != nil {
			return nil, &WorkerError{Kind: WorkerInternal, Message: "undecodable worker result"}
		}
		return &res, nil
	}

	var errBody protocol.ErrorBody
	if err := json.NewDecoder(limited).Decode(&errBody); err != nil {
		return nil, &WorkerError{Kind: WorkerInternal, Message: fmt.Sprintf("worker status %d", resp.StatusCode)}
	}
	msg := errBody.Message
	if len(msg) > maxWorkerMessageBytes {
		msg = msg[:maxWorkerMessageBytes]
	}

	switch errBody.Kind {
	case protocol.KindTimeout:
		return nil, fmt.Errorf("%w: %s", ErrCallTimeout, msg)
	case protocol.KindBadRequest:
		return nil, fmt.Errorf("%w: worker rejected the request: %s", ErrUnsupportedRequest, msg)
	case protocol.KindUnauthorized, protocol.KindAuth:
		return nil, &WorkerError{Kind: WorkerAuth, Message: msg}
	case protocol.KindRateLimited, protocol.KindOverloaded, protocol.KindUnavailable, protocol.KindInternal:
		return nil, &WorkerError{Kind: errBody.Kind, Message: msg}
	default:
		return nil, &WorkerError{Kind: WorkerInternal, Message: fmt.Sprintf("worker status %d", resp.StatusCode)}
	}
}
