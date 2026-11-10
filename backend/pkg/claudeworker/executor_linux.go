package claudeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"pentagi/pkg/claudeworker/protocol"
)

const (
	maxSchemaBytes = 100 << 10
	maxStderrBytes = 64 << 10
	emptyMCPConfig = `{"mcpServers":{}}`
)

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,127}$`)

// isolationEnv switches off everything in the CLI that could read local state,
// reach a network service other than the model, or persist the conversation.
var isolationEnv = []string{
	"DISABLE_TELEMETRY=1",
	"DISABLE_ERROR_REPORTING=1",
	"DISABLE_AUTOUPDATER=1",
	"DISABLE_UPDATES=1",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
	"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
	"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1",
	"CLAUDE_CODE_DISABLE_WEB_FETCH=1",
	"CLAUDE_CODE_DISABLE_BUNDLED_SKILLS=1",
	"CLAUDE_CODE_SKIP_PROMPT_HISTORY=1",
}

var geteuid = os.Geteuid

type Executor struct {
	cfg       Config
	sem       chan struct{}
	ownerHome string
}

func NewExecutor(cfg Config) (*Executor, error) {
	if err := CheckNotRoot(geteuid()); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	ex := &Executor{cfg: cfg, sem: make(chan struct{}, cfg.MaxConcurrent)}
	if cfg.UseOwnerLogin {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return nil, errors.New("owner login needs the worker user's home directory")
		}
		ex.ownerHome = home
	}
	return ex, nil
}

func validateRequest(req protocol.Request) error {
	switch {
	case req.Version != protocol.Version:
		return fmt.Errorf("unsupported protocol version %d", req.Version)
	case !modelPattern.MatchString(req.Model):
		return errors.New("invalid model name")
	case strings.TrimSpace(req.Transcript) == "":
		return errors.New("empty transcript")
	case req.TimeoutMS <= 0:
		return errors.New("timeout must be positive")
	case len(req.OutputSchema) > maxSchemaBytes:
		return fmt.Errorf("output schema exceeds %d bytes", maxSchemaBytes)
	}
	if len(req.OutputSchema) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(req.OutputSchema, &schema); err != nil || schema == nil {
			return errors.New("output schema is not a JSON object")
		}
	}
	return nil
}

// Run executes one request. On success the result may still describe a CLI-level
// failure (IsError); judging it, including the tool evidence, is the adapter's job.
func (e *Executor) Run(ctx context.Context, req protocol.Request) (*protocol.Result, error) {
	if err := validateRequest(req); err != nil {
		return nil, &Error{Kind: protocol.KindBadRequest, Message: err.Error()}
	}

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	default:
		return nil, &Error{Kind: protocol.KindOverloaded, Message: "all CLI slots are busy"}
	}

	timeout := min(time.Duration(req.TimeoutMS)*time.Millisecond, e.cfg.MaxTimeout)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp(e.cfg.WorkRoot, "call-")
	if err != nil {
		return nil, &Error{Kind: protocol.KindInternal, Message: "cannot create call directory"}
	}
	defer os.RemoveAll(dir)

	cmd, err := e.command(dir, req)
	if err != nil {
		return nil, err
	}

	var capture io.Writer = io.Discard
	if e.cfg.CaptureDir != "" {
		f, err := os.OpenFile(filepath.Join(e.cfg.CaptureDir, filepath.Base(dir)+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, &Error{Kind: protocol.KindInternal, Message: "cannot create capture file"}
		}
		defer f.Close()
		capture = f
	}

	res, err := e.execute(callCtx, cmd, capture)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if callCtx.Err() != nil {
		return nil, &Error{Kind: protocol.KindTimeout, Message: fmt.Sprintf("CLI did not finish within %s", timeout)}
	}
	if err != nil {
		return nil, err
	}
	res.CLIVersion = e.cfg.CLIVersion
	return res, nil
}

func (e *Executor) command(dir string, req protocol.Request) (*exec.Cmd, error) {
	paths := map[string]string{}
	for _, sub := range []string{"home", "config", "cwd", "tmp"} {
		p := filepath.Join(dir, sub)
		if err := os.Mkdir(p, 0o700); err != nil {
			return nil, &Error{Kind: protocol.KindInternal, Message: "cannot prepare call directory"}
		}
		paths[sub] = p
	}

	promptPath := filepath.Join(dir, "system-prompt.txt")
	if err := os.WriteFile(promptPath, []byte(req.SystemPrompt), 0o600); err != nil {
		return nil, &Error{Kind: protocol.KindInternal, Message: "cannot write system prompt"}
	}

	env, err := e.childEnv(paths)
	if err != nil {
		return nil, err
	}

	args := buildArgs(req.Model, promptPath, req.OutputSchema)
	if e.cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(e.cfg.MaxTurns))
	}
	cmd := exec.Command(e.cfg.CLIPath, args...)
	cmd.Dir = paths["cwd"]
	cmd.Env = env
	cmd.Stdin = strings.NewReader(req.Transcript)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = e.cfg.KillGrace
	return cmd, nil
}

// buildArgs fixes every flag in code; the request contributes only the model name
// (pattern-checked), a file path the worker chose, and the schema.
func buildArgs(model, promptPath string, schema json.RawMessage) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--restricted",
		"--tools", "",
		"--strict-mcp-config",
		"--mcp-config", emptyMCPConfig,
		"--disallowedTools", "mcp__*",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--model", model,
		"--system-prompt-file", promptPath,
	}
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	}
	return args
}

// childEnv is built from nothing: the worker's own environment, including any
// stray API key, never reaches the CLI.
func (e *Executor) childEnv(paths map[string]string) ([]string, error) {
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8",
		"TMPDIR=" + paths["tmp"],
	}
	if e.cfg.UseOwnerLogin {
		env = append(env, "HOME="+e.ownerHome)
	} else {
		env = append(env, "HOME="+paths["home"], "CLAUDE_CONFIG_DIR="+paths["config"])
	}
	env = append(env, isolationEnv...)
	if e.cfg.HTTPSProxy != "" {
		env = append(env, "HTTPS_PROXY="+e.cfg.HTTPSProxy)
	}

	if e.cfg.OAuthTokenFile != "" {
		raw, err := os.ReadFile(e.cfg.OAuthTokenFile)
		token := strings.TrimSpace(string(raw))
		if err != nil || token == "" || strings.ContainsAny(token, "\n\r\x00") {
			return nil, &Error{Kind: protocol.KindAuth, Message: "CLI credential is unavailable"}
		}
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+token)
	}
	return env, nil
}

type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.limit - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *cappedBuffer) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Len()
}

// execute owns the process group from start to finish. Every exit path ends with
// SIGKILL to the whole group, so a grandchild that kept the group cannot outlive
// the call. A process that leaves the group with setsid is out of its reach: the
// call still ends, because the reader is closed, but that process lives on until
// the systemd unit's control-group kill.
func (e *Executor) execute(ctx context.Context, cmd *exec.Cmd, capture io.Writer) (*protocol.Result, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, &Error{Kind: protocol.KindInternal, Message: "cannot create output pipe"}
	}
	defer outR.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		outW.Close()
		return nil, &Error{Kind: protocol.KindInternal, Message: "cannot create error pipe"}
	}
	defer errR.Close()
	cmd.Stdout, cmd.Stderr = outW, errW

	startErr := cmd.Start()
	outW.Close()
	errW.Close()
	if startErr != nil {
		return nil, &Error{Kind: protocol.KindUnavailable, Message: "cannot start the CLI"}
	}
	pgid := cmd.Process.Pid
	killGroup := func(sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }
	defer killGroup(syscall.SIGKILL)

	stderr := &cappedBuffer{limit: maxStderrBytes}
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stderr, errR)
		close(stderrDone)
	}()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	finished := make(chan struct{})
	reaperDone := make(chan struct{})
	// unblockReader closes the read end if output still has not ended a grace period
	// after the group was killed: a process that escaped the group can hold the
	// write end open forever.
	unblockReader := func() {
		select {
		case <-finished:
		case <-time.After(e.cfg.KillGrace):
			outR.Close()
		}
	}
	go func() {
		defer close(reaperDone)
		select {
		case <-ctx.Done():
			killGroup(syscall.SIGTERM)
			select {
			case <-finished:
			case <-time.After(e.cfg.KillGrace):
			}
			killGroup(syscall.SIGKILL)
			unblockReader()
		case err := <-exited:
			exited <- err
			select {
			case <-finished:
			case <-ctx.Done():
			case <-time.After(e.cfg.KillGrace):
			}
			killGroup(syscall.SIGKILL)
			unblockReader()
		case <-finished:
		}
	}()

	res, parseErr := parseStream(io.TeeReader(outR, capture), e.cfg.MaxOutputBytes)
	if parseErr != nil {
		killGroup(syscall.SIGKILL)
	}
	close(finished)
	<-reaperDone
	killGroup(syscall.SIGKILL)
	outR.Close()
	waitErr := <-exited
	errR.Close()
	<-stderrDone

	switch {
	case errors.Is(parseErr, errOutputTooLarge):
		return nil, &Error{Kind: protocol.KindInternal, Message: fmt.Sprintf("CLI output exceeded %d bytes", e.cfg.MaxOutputBytes)}
	case parseErr != nil:
		return nil, &Error{Kind: protocol.KindInternal, Message: "CLI output was not valid stream-json"}
	case res == nil:
		return nil, &Error{Kind: protocol.KindInternal, Message: fmt.Sprintf(
			"CLI ended without a result (%s, %d bytes on stderr)", exitDescription(waitErr), stderr.Len())}
	}
	return res, nil
}

func exitDescription(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return "killed by " + status.Signal().String()
		}
		return fmt.Sprintf("exit status %d", exitErr.ExitCode())
	}
	if err != nil {
		return "wait failed"
	}
	return "exit status 0"
}
