package claudecli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"pentagi/pkg/config"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
)

func TestDoor_DefaultProviderConfig_UsesTheCLIAlias(t *testing.T) {
	cfg, err := DefaultProviderConfig()
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{config: cfg, defaultModel: "sonnet"}
	for _, opt := range []pconfig.ProviderOptionsType{pconfig.OptionsTypeSimple, pconfig.OptionsTypePentester, pconfig.OptionsTypePrimaryAgent} {
		if got := p.Model(opt); got != "sonnet" {
			t.Errorf("%s model = %q, want sonnet", opt, got)
		}
	}
}

func TestDoor_NewFromConfig_RejectsIncompleteSettings(t *testing.T) {
	cases := []*config.Config{
		{},
		{ClaudeCLIWorkerURL: "http://127.0.0.1:8765"},
		{ClaudeCLIWorkerToken: "tok"},
		{ClaudeCLIWorkerURL: "unix://relative/worker.sock", ClaudeCLIWorkerToken: "tok"},
		{ClaudeCLIWorkerURL: "ftp://127.0.0.1", ClaudeCLIWorkerToken: "tok"},
	}
	pc, _ := DefaultProviderConfig()
	for _, cfg := range cases {
		if _, err := NewFromConfig(cfg, provider.DefaultProviderNameClaudeCLI, pc); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
}

// The provider the registry builds reaches a worker over its Unix socket and
// returns a validated tool call.
func TestDoor_NewFromConfig_ReachesTheWorkerOverAUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "cw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "w.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var got Request
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer door-test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(structured(terminalCall))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cfg := &config.Config{ClaudeCLIWorkerURL: "unix://" + sock, ClaudeCLIWorkerToken: "door-test-token", ClaudeCLICallTimeout: 30}
	pc, err := DefaultProviderConfig()
	if err != nil {
		t.Fatal(err)
	}
	prv, err := NewFromConfig(cfg, provider.DefaultProviderNameClaudeCLI, pc)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if prv.Type() != provider.ProviderClaudeCLI || prv.Name() != "claude_cli" {
		t.Errorf("type %q name %q", prv.Type(), prv.Name())
	}

	resp, err := prv.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
		pentagiTools(t, "terminal"), nil)
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	if calls := resp.Choices[0].ToolCalls; len(calls) != 1 || calls[0].FunctionCall.Name != "terminal" {
		t.Errorf("tool calls = %+v", calls)
	}
	if got.Model != "sonnet" || got.Agent != "pentester" || got.TimeoutMS != 30_000 {
		t.Errorf("worker received model=%q agent=%q timeout=%d", got.Model, got.Agent, got.TimeoutMS)
	}
}
