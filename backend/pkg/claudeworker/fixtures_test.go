package claudeworker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"pentagi/pkg/claudeworker/protocol"
)

// The fake CLI is this test binary re-entered through a shell script, so no real
// claude binary is ever involved. The script fixes the mode and the record
// directory because the worker hands the CLI an environment built from nothing.
const (
	fakeModeEnv   = "CLAUDEWORKER_FAKE_MODE"
	fakeRecordEnv = "CLAUDEWORKER_FAKE_RECORD"
	fakeToken     = "fake-oauth-token-for-tests-only-0123456789"
)

const decisionOutput = `{"kind":"tool_calls","calls":[{"name":"terminal","arguments":` +
	`{"input":"id","cwd":"/work","detach":false,"timeout":0,"message":"check the current user"}}]}`

type fakeRecord struct {
	Args         []string `json:"args"`
	Env          []string `json:"env"`
	UID          int      `json:"uid"`
	EUID         int      `json:"euid"`
	Cwd          string   `json:"cwd"`
	Stdin        string   `json:"stdin"`
	SystemPrompt string   `json:"system_prompt"`
	PID          int      `json:"pid"`
	PGID         int      `json:"pgid"`
	ChildPID     int      `json:"child_pid"`
}

func TestExecutor_FakeCLIProcess(t *testing.T) {
	mode := os.Getenv(fakeModeEnv)
	if mode == "" {
		t.Skip("runs only as the fake CLI child process")
	}
	os.Exit(runFakeCLI(mode, os.Getenv(fakeRecordEnv), fakeArgs()))
}

func fakeArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

func emit(lines ...string) {
	for _, l := range lines {
		fmt.Fprintln(os.Stdout, l)
	}
}

func initEvent(tools string) string {
	return `{"type":"system","subtype":"init","tools":` + tools + `,"mcp_servers":[]}`
}

func resultEvent(structured string) string {
	return `{"type":"result","subtype":"success","is_error":false,"num_turns":2,"stop_reason":"end_turn",` +
		`"structured_output":` + structured + `,"total_cost_usd":0.0042,"permission_denials":[],` +
		`"usage":{"input_tokens":900,"output_tokens":70,"cache_read_input_tokens":12,"cache_creation_input_tokens":3}}`
}

func structuredToolUse() string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"StructuredOutput","input":{}}]}}`
}

func startSleeper(holdStdout bool) int {
	cmd := exec.Command("/bin/sleep", "300")
	if holdStdout {
		cmd.Stdout = os.Stdout
	}
	if err := cmd.Start(); err != nil {
		return 0
	}
	return cmd.Process.Pid
}

// startEscapedSleeper starts a child in a new session that keeps stdout open, the
// way a misbehaving CLI could leave the worker's process group.
func startEscapedSleeper() int {
	cmd := exec.Command("/bin/sleep", "300")
	cmd.Stdout = os.Stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0
	}
	return cmd.Process.Pid
}

func runFakeCLI(mode, recordDir string, args []string) int {
	stdin, _ := readAll(os.Stdin)
	rec := fakeRecord{Args: args, UID: os.Getuid(), EUID: os.Geteuid(), Stdin: stdin, PID: os.Getpid()}
	rec.PGID, _ = syscall.Getpgid(0)
	rec.Cwd, _ = os.Getwd()
	for _, kv := range os.Environ() {
		if !fakeOwnEnv(kv) {
			rec.Env = append(rec.Env, kv)
		}
	}
	for i, a := range args {
		if a == "--system-prompt-file" && i+1 < len(args) {
			b, _ := os.ReadFile(args[i+1])
			rec.SystemPrompt = string(b)
		}
	}

	switch mode {
	case "hang_tree", "lingering_child", "no_result_lingering":
		rec.ChildPID = startSleeper(mode != "hang_tree")
	case "escape_session":
		rec.ChildPID = startEscapedSleeper()
	}
	writeRecord(recordDir, rec)

	switch mode {
	case "decision":
		emit(initEvent(`["StructuredOutput"]`), structuredToolUse(),
			`{"type":"user","message":{"content":"structured output accepted"}}`, resultEvent(decisionOutput))
	case "text":
		emit(initEvent(`[]`), `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"fake answer",`+
			`"usage":{"input_tokens":10,"output_tokens":2}}`)
	case "slow":
		time.Sleep(2 * time.Second)
		emit(initEvent(`["StructuredOutput"]`), resultEvent(decisionOutput))
	case "hang_tree":
		emit(initEvent(`[]`))
		select {}
	case "ignore_term":
		signal.Ignore(syscall.SIGTERM)
		emit(initEvent(`[]`))
		select {}
	case "lingering_child":
		emit(initEvent(`["StructuredOutput"]`), resultEvent(decisionOutput))
	case "no_result_lingering", "escape_session":
		emit(initEvent(`[]`))
	case "malformed":
		emit("this is not stream-json")
	case "no_result":
		emit(initEvent(`[]`))
	case "crash":
		emit(initEvent(`[]`))
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	case "error_result":
		emit(initEvent(`[]`), `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"],"num_turns":1}`)
		return 1
	case "native_tool":
		emit(`{"type":"system","subtype":"init","tools":["Bash","StructuredOutput"],"mcp_servers":[{"name":"evil","status":"connected"}]}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"x"},{"type":"tool_use","name":"Bash","input":{}},`+
				`{"type":"server_tool_use","name":"web_search","input":{}}]}}`,
			`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"structured_output":`+decisionOutput+
				`,"permission_denials":[{"tool_name":"Bash"}]}`)
	case "flood":
		line := `{"type":"system","subtype":"noise","pad":"` + strings.Repeat("x", 64<<10) + `"}`
		for range 64 {
			emit(line)
		}
		select {}
	case "leak_secret":
		token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
		fmt.Fprintln(os.Stderr, "debug token="+token)
		emit("token=" + token)
		return 3
	default:
		fmt.Fprintln(os.Stderr, "unknown fake mode "+mode)
		return 2
	}
	return 0
}

// fakeOwnEnv drops what the wrapper script adds, its own settings and the
// variables /bin/sh exports, so the record shows what the worker passed.
func fakeOwnEnv(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	switch name {
	case "PWD", "OLDPWD", "SHLVL", "_":
		return true
	}
	return strings.HasPrefix(name, "CLAUDEWORKER_FAKE_")
}

func readAll(f *os.File) (string, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(f)
	return b.String(), err
}

func writeRecord(dir string, rec fakeRecord) {
	if dir == "" {
		return
	}
	data, _ := json.Marshal(rec)
	tmp := filepath.Join(dir, fmt.Sprintf(".%d.tmp", rec.PID))
	_ = os.WriteFile(tmp, data, 0o600)
	_ = os.Rename(tmp, filepath.Join(dir, fmt.Sprintf("%d.json", rec.PID)))
}

type fakeCLI struct {
	executor  *Executor
	recordDir string
	workRoot  string
	tokenFile string
}

// newFakeExecutor builds an Executor whose CLI is the fake in the given mode.
func newFakeExecutor(t *testing.T, mode string, mutate ...func(*Config)) *fakeCLI {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("the worker refuses to run as root")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("test binary path: %v", err)
	}

	base := t.TempDir()
	dirs := map[string]string{}
	for _, d := range []string{"bin", "records", "work", "secrets"} {
		dirs[d] = filepath.Join(base, d)
		if err := os.Mkdir(dirs[d], 0o700); err != nil {
			t.Fatal(err)
		}
	}

	script := filepath.Join(dirs["bin"], "claude")
	body := fmt.Sprintf("#!/bin/sh\n%s='%s' %s='%s' exec '%s' -test.run='^TestExecutor_FakeCLIProcess$' -- \"$@\"\n",
		fakeModeEnv, mode, fakeRecordEnv, dirs["records"], self)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dirs["secrets"], "oauth-token")
	if err := os.WriteFile(tokenFile, []byte(fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		CLIPath:        script,
		CLIVersion:     "fake-1.0",
		WorkRoot:       dirs["work"],
		OAuthTokenFile: tokenFile,
		MaxConcurrent:  4,
		MaxTimeout:     30 * time.Second,
		KillGrace:      300 * time.Millisecond,
		MaxOutputBytes: 1 << 20,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	ex, err := NewExecutor(cfg)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return &fakeCLI{executor: ex, recordDir: dirs["records"], workRoot: dirs["work"], tokenFile: tokenFile}
}

func runRequest(transcript string) protocol.Request {
	return protocol.Request{
		Version:      protocol.Version,
		RequestID:    "req-1",
		Agent:        "pentester",
		Model:        "claude-sonnet-5",
		SystemPrompt: "SYSTEM-PROMPT-SENTINEL",
		Transcript:   transcript,
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		TimeoutMS:    20_000,
	}
}

// records waits until n fake processes have written their record.
func (f *fakeCLI) records(t *testing.T, n int) []fakeRecord {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		entries, _ := filepath.Glob(filepath.Join(f.recordDir, "*.json"))
		if len(entries) >= n {
			out := make([]fakeRecord, 0, len(entries))
			for _, e := range entries {
				var rec fakeRecord
				data, err := os.ReadFile(e)
				if err != nil || json.Unmarshal(data, &rec) != nil {
					t.Fatalf("bad record %s", e)
				}
				out = append(out, rec)
			}
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d fake CLI records appeared", len(entries), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alive treats a zombie as dead: it no longer runs, it only waits to be reaped.
func alive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+2 >= len(data) {
		return false
	}
	state := data[i+2]
	return state != 'Z' && state != 'X'
}

func requireGone(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range pids {
		if pid == 0 {
			t.Fatal("a process the fake CLI should have started has no pid")
		}
		for alive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d is still running", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s still holds %d entries after the call", dir, len(entries))
	}
}
