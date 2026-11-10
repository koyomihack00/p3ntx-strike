package claudeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"pentagi/pkg/claudeworker/protocol"
	"pentagi/pkg/providers/anthropic"
	"pentagi/pkg/providers/claudecli"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/tools"

	"github.com/sirupsen/logrus"
	"github.com/vxcontrol/langchaingo/llms"
)

func workerError(t *testing.T, err error, kind protocol.ErrorKind) *Error {
	t.Helper()
	var werr *Error
	if !errors.As(err, &werr) || werr.Kind != kind {
		t.Fatalf("err = %v, want worker error of kind %s", err, kind)
	}
	return werr
}

func TestExecutor_Run_ReturnsResultWithEvidence(t *testing.T) {
	f := newFakeExecutor(t, "decision")

	res, err := f.executor.Run(context.Background(), runRequest("<user-x>hello</user-x>"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Subtype != "success" || res.IsError || res.NumTurns != 2 || res.CLIVersion != "fake-1.0" {
		t.Errorf("result = %+v", res)
	}
	if !slices.Equal(res.InitTools, []string{"StructuredOutput"}) || !slices.Equal(res.ToolUses, []string{"StructuredOutput"}) {
		t.Errorf("evidence init=%v uses=%v", res.InitTools, res.ToolUses)
	}
	if mustCompact(t, string(res.StructuredOutput)) != mustCompact(t, decisionOutput) {
		t.Errorf("structured output = %s", res.StructuredOutput)
	}
	if res.Usage != (protocol.Usage{InputTokens: 900, OutputTokens: 70, CacheReadInputTokens: 12, CacheCreationInputTokens: 3}) {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.TotalCostUSD != 0.0042 || res.PermissionDenials != 0 {
		t.Errorf("cost=%v denials=%d", res.TotalCostUSD, res.PermissionDenials)
	}
	requireEmptyDir(t, f.workRoot)
}

func mustCompact(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestExecutor_Run_TranscriptGoesOnStdinWithFixedFlags(t *testing.T) {
	f := newFakeExecutor(t, "decision")
	transcript := "<user-x>TRANSCRIPT-SENTINEL scan 10.10.10.5</user-x>"
	req := runRequest(transcript)
	req.OutputSchema = json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string"}}}`)

	if _, err := f.executor.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec := f.records(t, 1)[0]

	if rec.Stdin != transcript {
		t.Errorf("stdin = %q", rec.Stdin)
	}
	if strings.Contains(strings.Join(rec.Args, " ")+strings.Join(rec.Env, " "), "TRANSCRIPT-SENTINEL") {
		t.Error("transcript leaked into argv or the environment")
	}
	if rec.SystemPrompt != "SYSTEM-PROMPT-SENTINEL" {
		t.Errorf("system prompt file held %q", rec.SystemPrompt)
	}
	promptIdx := slices.Index(rec.Args, "--system-prompt-file")
	if promptIdx < 0 {
		t.Fatalf("no --system-prompt-file in %q", rec.Args)
	}
	promptPath := rec.Args[promptIdx+1]
	want := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--restricted",
		"--tools", "",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--disallowedTools", "mcp__*",
		"--disable-slash-commands", "--no-session-persistence",
		"--model", "claude-sonnet-5",
		"--system-prompt-file", promptPath,
		"--json-schema", `{"type":"object","properties":{"kind":{"type":"string"}}}`,
	}
	if !slices.Equal(rec.Args, want) {
		t.Errorf("argv:\n%q\nwant:\n%q", rec.Args, want)
	}
	if _, err := os.Stat(promptPath); !os.IsNotExist(err) {
		t.Errorf("system prompt file survived the call: %v", err)
	}
}

func TestExecutor_Run_ChildEnvironmentIsBuiltFromScratch(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-must-not-leak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret-must-not-leak")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("HTTPS_PROXY", "http://203.0.113.9:8080")
	f := newFakeExecutor(t, "decision")

	if _, err := f.executor.Run(context.Background(), runRequest("hello")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec := f.records(t, 1)[0]

	env := map[string]string{}
	var keys []string
	for _, kv := range rec.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
		keys = append(keys, k)
	}
	slices.Sort(keys)
	wantKeys := []string{
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS", "CLAUDE_CODE_DISABLE_BUNDLED_SKILLS",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_DISABLE_WEB_FETCH",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_SKIP_PROMPT_HISTORY", "CLAUDE_CONFIG_DIR",
		"DISABLE_AUTOUPDATER", "DISABLE_ERROR_REPORTING", "DISABLE_TELEMETRY", "DISABLE_UPDATES",
		"HOME", "LANG", "PATH", "TMPDIR",
	}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("child env keys:\n%q\nwant:\n%q", keys, wantKeys)
	}
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != fakeToken {
		t.Error("child did not receive the configured credential")
	}
	for _, k := range []string{"HOME", "CLAUDE_CONFIG_DIR", "TMPDIR"} {
		if !strings.HasPrefix(env[k], f.workRoot+string(filepath.Separator)) {
			t.Errorf("%s = %q, want a directory under the work root", k, env[k])
		}
	}
	if !strings.HasPrefix(rec.Cwd, f.workRoot+string(filepath.Separator)) || filepath.Base(rec.Cwd) != "cwd" {
		t.Errorf("cwd = %q", rec.Cwd)
	}
	if rec.UID == 0 || rec.EUID == 0 || rec.UID != os.Getuid() {
		t.Errorf("child uid=%d euid=%d, want the unprivileged test user %d", rec.UID, rec.EUID, os.Getuid())
	}
	if rec.PGID != rec.PID {
		t.Errorf("child pgid=%d pid=%d: the CLI must lead its own process group", rec.PGID, rec.PID)
	}
	requireEmptyDir(t, f.workRoot)
}

func TestExecutor_Run_ProxyIsTheOnlyAddedVariable(t *testing.T) {
	f := newFakeExecutor(t, "decision", func(c *Config) { c.HTTPSProxy = "http://127.0.0.1:3128" })

	if _, err := f.executor.Run(context.Background(), runRequest("hello")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var proxies []string
	for _, kv := range f.records(t, 1)[0].Env {
		if name, _, _ := strings.Cut(kv, "="); strings.Contains(strings.ToLower(name), "proxy") {
			proxies = append(proxies, kv)
		}
	}
	if !slices.Equal(proxies, []string{"HTTPS_PROXY=http://127.0.0.1:3128"}) {
		t.Errorf("proxy variables = %q", proxies)
	}
}

func TestExecutor_Run_OwnerLoginUsesTheRealHomeAndNoTokenVariable(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory for the test user")
	}
	f := newFakeExecutor(t, "decision", func(c *Config) {
		c.OAuthTokenFile = ""
		c.UseOwnerLogin = true
	})

	if _, err := f.executor.Run(context.Background(), runRequest("hello")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := map[string]string{}
	for _, kv := range f.records(t, 1)[0].Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["HOME"] != home {
		t.Errorf("HOME = %q, want the owner's %q", env["HOME"], home)
	}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s is set in owner-login mode", k)
		}
	}
	if !strings.HasPrefix(env["TMPDIR"], f.workRoot+string(filepath.Separator)) {
		t.Errorf("TMPDIR = %q, want a per-call directory", env["TMPDIR"])
	}
}

func TestExecutor_Run_MaxTurnsIsPassedOnlyWhenSet(t *testing.T) {
	f := newFakeExecutor(t, "decision", func(c *Config) { c.MaxTurns = 3 })
	if _, err := f.executor.Run(context.Background(), runRequest("hello")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	args := f.records(t, 1)[0].Args
	if got := args[len(args)-2:]; !slices.Equal(got, []string{"--max-turns", "3"}) {
		t.Errorf("argv tail = %q", got)
	}
}

func TestExecutor_Run_CaptureKeepsTheRawStream(t *testing.T) {
	captureDir := t.TempDir()
	if err := os.Chmod(captureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := newFakeExecutor(t, "decision", func(c *Config) { c.CaptureDir = captureDir })

	if _, err := f.executor.Run(context.Background(), runRequest("hello")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(captureDir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("capture files = %v, want 1", files)
	}
	data, _ := os.ReadFile(files[0])
	info, _ := os.Stat(files[0])
	if !strings.Contains(string(data), `"subtype":"init"`) || !strings.Contains(string(data), `"type":"result"`) {
		t.Errorf("capture lacks the stream: %s", data)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("capture mode %v, want 0600", info.Mode().Perm())
	}
}

func TestExecutor_Run_CallsShareNoState(t *testing.T) {
	f := newFakeExecutor(t, "decision")
	for range 2 {
		if _, err := f.executor.Run(context.Background(), runRequest("hello")); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	recs := f.records(t, 2)
	home := func(r fakeRecord) string {
		for _, kv := range r.Env {
			if v, ok := strings.CutPrefix(kv, "HOME="); ok {
				return v
			}
		}
		return ""
	}
	if home(recs[0]) == home(recs[1]) {
		t.Errorf("two calls shared HOME %q", home(recs[0]))
	}
	requireEmptyDir(t, f.workRoot)
}

func TestExecutor_Run_CancellationKillsTheProcessTree(t *testing.T) {
	f := newFakeExecutor(t, "hang_tree")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.executor.Run(ctx, runRequest("hello"))
		done <- err
	}()

	rec := f.records(t, 1)[0]
	if !alive(rec.PID) || !alive(rec.ChildPID) {
		t.Fatal("fake CLI tree is not running before cancellation")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	requireGone(t, rec.PID, rec.ChildPID)
	requireEmptyDir(t, f.workRoot)
}

func TestExecutor_Run_EscalatesToSIGKILLWhenTermIsIgnored(t *testing.T) {
	f := newFakeExecutor(t, "ignore_term")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.executor.Run(ctx, runRequest("hello"))
		done <- err
	}()
	rec := f.records(t, 1)[0]
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	requireGone(t, rec.PID)
}

func TestExecutor_Run_TimeoutKillsTheProcessTree(t *testing.T) {
	f := newFakeExecutor(t, "hang_tree")
	req := runRequest("hello")
	req.TimeoutMS = 5000

	_, err := f.executor.Run(context.Background(), req)
	workerError(t, err, protocol.KindTimeout)
	rec := f.records(t, 1)[0]
	requireGone(t, rec.PID, rec.ChildPID)
}

func TestExecutor_Run_MaxTimeoutCapsTheRequest(t *testing.T) {
	f := newFakeExecutor(t, "hang_tree", func(c *Config) { c.MaxTimeout = 400 * time.Millisecond })

	start := time.Now()
	_, err := f.executor.Run(context.Background(), runRequest("hello"))
	workerError(t, err, protocol.KindTimeout)
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("a 400ms cap took %s", elapsed)
	}
}

func TestExecutor_Run_LingeringChildIsKilledAfterTheResult(t *testing.T) {
	f := newFakeExecutor(t, "lingering_child")

	res, err := f.executor.Run(context.Background(), runRequest("hello"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Subtype != "success" {
		t.Errorf("subtype = %q", res.Subtype)
	}
	rec := f.records(t, 1)[0]
	requireGone(t, rec.ChildPID)
}

func TestExecutor_Run_FailsClosedOnBadOutput(t *testing.T) {
	for _, mode := range []string{"malformed", "no_result", "crash", "flood", "no_result_lingering", "leak_secret"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeExecutor(t, mode, func(c *Config) { c.MaxOutputBytes = 256 << 10 })

			res, err := runWithin(t, f.executor, 30*time.Second)
			werr := workerError(t, err, protocol.KindInternal)
			if res != nil {
				t.Errorf("a result escaped: %+v", res)
			}
			if strings.Contains(werr.Error(), fakeToken) {
				t.Error("the credential leaked into the error")
			}
			if rec := f.records(t, 1)[0]; rec.ChildPID != 0 {
				requireGone(t, rec.ChildPID)
			}
			requireEmptyDir(t, f.workRoot)
		})
	}
}

// runWithin fails the test instead of hanging when Run never returns.
func runWithin(t *testing.T, ex *Executor, limit time.Duration) (*protocol.Result, error) {
	t.Helper()
	type outcome struct {
		res *protocol.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := ex.Run(context.Background(), runRequest("hello"))
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(limit):
		t.Fatalf("Run did not return within %s", limit)
		return nil, nil
	}
}

func TestExecutor_Run_EscapedProcessCannotHoldTheCallOpen(t *testing.T) {
	f := newFakeExecutor(t, "escape_session")

	res, err := runWithin(t, f.executor, 30*time.Second)
	rec := f.records(t, 1)[0]
	t.Cleanup(func() { _ = syscall.Kill(rec.ChildPID, syscall.SIGKILL) })
	workerError(t, err, protocol.KindInternal)
	if res != nil {
		t.Errorf("a result escaped: %+v", res)
	}
	requireGone(t, rec.PID)
}

func TestExecutor_Run_ErrorResultIsReturnedForTheAdapterToJudge(t *testing.T) {
	f := newFakeExecutor(t, "error_result")

	res, err := f.executor.Run(context.Background(), runRequest("hello"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError || res.Subtype != "error_during_execution" || !slices.Equal(res.Errors, []string{"boom"}) {
		t.Errorf("result = %+v", res)
	}
}

func TestExecutor_Run_ReportsNativeToolEvidence(t *testing.T) {
	f := newFakeExecutor(t, "native_tool")

	res, err := f.executor.Run(context.Background(), runRequest("hello"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(res.InitTools, []string{"Bash", "StructuredOutput", "mcp_server:evil"}) {
		t.Errorf("init tools = %q", res.InitTools)
	}
	if !slices.Equal(res.ToolUses, []string{"Bash", "web_search"}) || res.PermissionDenials != 1 {
		t.Errorf("tool uses = %q, denials = %d", res.ToolUses, res.PermissionDenials)
	}
}

func TestExecutor_Run_RejectsInvalidRequestsWithoutStartingTheCLI(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*protocol.Request)
	}{
		{"protocol version", func(r *protocol.Request) { r.Version = 2 }},
		{"flag as model", func(r *protocol.Request) { r.Model = "--dangerously-skip-permissions" }},
		{"model with space", func(r *protocol.Request) { r.Model = "claude sonnet" }},
		{"empty transcript", func(r *protocol.Request) { r.Transcript = "  " }},
		{"no timeout", func(r *protocol.Request) { r.TimeoutMS = 0 }},
		{"schema not an object", func(r *protocol.Request) { r.OutputSchema = json.RawMessage(`[1]`) }},
		{"schema too large", func(r *protocol.Request) {
			r.OutputSchema = json.RawMessage(`{"description":"` + strings.Repeat("x", 101<<10) + `"}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeExecutor(t, "decision")
			req := runRequest("hello")
			tc.mutate(&req)

			_, err := f.executor.Run(context.Background(), req)
			workerError(t, err, protocol.KindBadRequest)
			if entries, _ := filepath.Glob(filepath.Join(f.recordDir, "*.json")); len(entries) != 0 {
				t.Error("the CLI was started for an invalid request")
			}
		})
	}
}

func TestExecutor_Run_RejectsWhenEverySlotIsBusy(t *testing.T) {
	f := newFakeExecutor(t, "hang_tree", func(c *Config) { c.MaxConcurrent = 1 })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = f.executor.Run(ctx, runRequest("hello"))
		close(done)
	}()
	rec := f.records(t, 1)[0]

	_, err := f.executor.Run(context.Background(), runRequest("hello"))
	workerError(t, err, protocol.KindOverloaded)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("first call did not end")
	}
	requireGone(t, rec.PID, rec.ChildPID)
}

func TestExecutor_Run_ConcurrentCallsAreIsolated(t *testing.T) {
	f := newFakeExecutor(t, "decision", func(c *Config) { c.MaxConcurrent = 4 })

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.executor.Run(context.Background(), runRequest("hello"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent call: %v", err)
		}
	}
	seen := map[string]bool{}
	for _, rec := range f.records(t, 4) {
		if seen[rec.Cwd] {
			t.Errorf("two calls shared cwd %s", rec.Cwd)
		}
		seen[rec.Cwd] = true
	}
	requireEmptyDir(t, f.workRoot)
}

func TestExecutor_Run_UnreadableCredentialFailsWithoutDetail(t *testing.T) {
	f := newFakeExecutor(t, "decision")
	if err := os.Remove(f.tokenFile); err != nil {
		t.Fatal(err)
	}

	_, err := f.executor.Run(context.Background(), runRequest("hello"))
	werr := workerError(t, err, protocol.KindAuth)
	if strings.Contains(werr.Message, f.tokenFile) {
		t.Error("the credential path leaked into the error")
	}
	if entries, _ := filepath.Glob(filepath.Join(f.recordDir, "*.json")); len(entries) != 0 {
		t.Error("the CLI was started without its credential")
	}
}

func TestExecutor_NewExecutor_RefusesRoot(t *testing.T) {
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = os.Geteuid })

	if _, err := NewExecutor(Config{}); !errors.Is(err, ErrRoot) {
		t.Errorf("err = %v, want ErrRoot", err)
	}
}

// endToEnd wires the real adapter to the real worker over HTTP, with the fake CLI
// at the far end, and captures the worker's log.
func endToEnd(t *testing.T, mode string) (*claudecli.Provider, *fakeCLI, *bytes.Buffer, string) {
	t.Helper()
	f := newFakeExecutor(t, mode)
	logBuf := &bytes.Buffer{}
	logger := logrus.New()
	logger.SetOutput(logBuf)
	logger.SetFormatter(&logrus.JSONFormatter{})

	const bearer = "worker-bearer-token-for-tests-0123456789"
	srv, err := NewServer(f.executor, bearer, logger)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	runner, err := claudecli.NewHTTPRunner(ts.URL, bearer, ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := anthropic.DefaultProviderConfig()
	if err != nil {
		t.Fatal(err)
	}
	models, err := anthropic.DefaultModels()
	if err != nil {
		t.Fatal(err)
	}
	p, err := claudecli.New(claudecli.Options{
		Name: "claude-cli-e2e", Config: cfg, Models: models, DefaultModel: "claude-sonnet-5",
		ToolCallIDTemplate: "toolu_{r:24:b}", Runner: runner, CallTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, f, logBuf, bearer
}

func e2eChain() []llms.MessageContent {
	return []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, "SYSTEM-PROMPT-SENTINEL"),
		llms.TextParts(llms.ChatMessageTypeHuman, "TRANSCRIPT-SENTINEL find the current user"),
	}
}

func terminalTool() []llms.Tool {
	def := tools.GetRegistryDefinitions()[tools.TerminalToolName]
	return []llms.Tool{{Type: "function", Function: &def}}
}

func TestExecutor_EndToEnd_AdapterGetsAValidatedToolCall(t *testing.T) {
	p, f, logBuf, bearer := endToEnd(t, "decision")

	resp, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, e2eChain(), terminalTool(), nil)
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	calls := resp.Choices[0].ToolCalls
	if len(calls) != 1 || calls[0].FunctionCall.Name != "terminal" || !strings.HasPrefix(calls[0].ID, "toolu_") {
		t.Fatalf("tool calls = %+v", calls)
	}
	if got := p.GetUsage(resp.Choices[0].GenerationInfo); got.Input != 900 || got.Output != 70 {
		t.Errorf("usage = %+v", got)
	}

	rec := f.records(t, 1)[0]
	if !strings.Contains(rec.Stdin, "TRANSCRIPT-SENTINEL") || !strings.Contains(rec.SystemPrompt, "SYSTEM-PROMPT-SENTINEL") {
		t.Error("the adapter's prompt did not reach the CLI")
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"outcome":"ok"`) {
		t.Errorf("no request log line: %s", logs)
	}
	for _, secret := range []string{"TRANSCRIPT-SENTINEL", "SYSTEM-PROMPT-SENTINEL", bearer, fakeToken, "check the current user"} {
		if strings.Contains(logs, secret) {
			t.Errorf("worker log contains %q", secret)
		}
	}
}

func TestExecutor_EndToEnd_NativeToolEvidenceIsRejectedByTheAdapter(t *testing.T) {
	p, _, _, _ := endToEnd(t, "native_tool")

	resp, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, e2eChain(), terminalTool(), nil)
	if !errors.Is(err, claudecli.ErrNativeToolActivity) || resp != nil {
		t.Errorf("resp=%v err=%v, want ErrNativeToolActivity", resp, err)
	}
}

func TestExecutor_EndToEnd_CancellationKillsTheProcessTree(t *testing.T) {
	p, f, _, _ := endToEnd(t, "hang_tree")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.CallWithTools(ctx, pconfig.OptionsTypePentester, e2eChain(), terminalTool(), nil)
		done <- err
	}()

	rec := f.records(t, 1)[0]
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CallWithTools did not return after cancellation")
	}
	requireGone(t, rec.PID, rec.ChildPID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, _ := os.ReadDir(f.workRoot)
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("work root still holds %d entries", len(entries))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecutor_EndToEnd_WorkerErrorsCarryNoSecrets(t *testing.T) {
	p, _, logBuf, bearer := endToEnd(t, "leak_secret")

	_, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, e2eChain(), terminalTool(), nil)
	if err == nil {
		t.Fatal("a broken CLI produced no error")
	}
	for _, secret := range []string{fakeToken, bearer, "TRANSCRIPT-SENTINEL"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(logBuf.String(), secret) {
			t.Errorf("%q leaked into the error or the log", secret)
		}
	}
}
