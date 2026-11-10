//go:build linux

// Command claudesmoke makes at most three benign calls through the claudecli
// adapter and the claudeworker executor against the locally installed Claude Code
// CLI, using the current user's existing login, and records the results. Proposed
// tool calls are only printed, never executed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pentagi/pkg/claudeworker"
	"pentagi/pkg/providers/claudecli"
	"pentagi/pkg/providers/pconfig"

	"github.com/vxcontrol/langchaingo/llms"
)

type callRecord struct {
	Name      string         `json:"name"`
	OK        bool           `json:"ok"`
	Error     string         `json:"error,omitempty"`
	LatencyMS int64          `json:"latency_ms"`
	Content   string         `json:"content,omitempty"`
	ToolCalls []toolCall     `json:"tool_calls,omitempty"`
	Info      map[string]any `json:"generation_info,omitempty"`
}

type toolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type report struct {
	CLIPath                string       `json:"cli_path"`
	CLIVersion             string       `json:"cli_version"`
	Model                  string       `json:"model"`
	AuthMethod             string       `json:"auth_method"`
	CredentialsMtimeBefore string       `json:"credentials_mtime_before"`
	CredentialsMtimeAfter  string       `json:"credentials_mtime_after"`
	Calls                  []callRecord `json:"calls"`
	TurnsUsed              int          `json:"turns_used"`
	Stopped                string       `json:"stopped,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "claudesmoke:", err)
		os.Exit(1)
	}
}

func credentialsMtime(home string) string {
	info, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return "absent"
	}
	return info.ModTime().UTC().Format(time.RFC3339Nano)
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func run() error {
	home, _ := os.UserHomeDir()
	cli := flag.String("cli", filepath.Join(home, ".local", "bin", "claude"), "Claude Code CLI path")
	version := flag.String("cli-version", "", "CLI version to record")
	model := flag.String("model", "sonnet", "model alias or name")
	out := flag.String("out", "", "private output directory for results and raw streams")
	only := flag.String("only", "", "run only the named step (plain_text, structured_json, dummy_tool_call)")
	maxTurns := flag.Int("max-turns", 0, "per-call --max-turns for the CLI (0 leaves it unset)")
	turnBudget := flag.Int("turn-budget", 8, "stop before starting a call once this many model turns are used")
	flag.Parse()
	if *out == "" {
		return errors.New("-out is required")
	}

	if err := claudeworker.CheckNotRoot(os.Geteuid()); err != nil {
		return err
	}
	if err := claudeworker.CheckEnvironment(os.LookupEnv); err != nil {
		return err
	}
	cliPath, err := filepath.EvalSymlinks(*cli)
	if err != nil {
		return fmt.Errorf("CLI not found: %w", err)
	}

	work, capture := filepath.Join(*out, "work"), filepath.Join(*out, "capture")
	for _, d := range []string{*out, work, capture} {
		if err := privateDir(d); err != nil {
			return err
		}
	}

	executor, err := claudeworker.NewExecutor(claudeworker.Config{
		CLIPath:       cliPath,
		CLIVersion:    *version,
		WorkRoot:      work,
		CaptureDir:    capture,
		UseOwnerLogin: true,
		MaxConcurrent: 1,
		MaxTurns:      *maxTurns,
		MaxTimeout:    3 * time.Minute,
		KillGrace:     3 * time.Second,
	})
	if err != nil {
		return err
	}

	yaml := fmt.Sprintf("simple:\n  model: %[1]s\nsimple_json:\n  model: %[1]s\n  json: true\npentester:\n  model: %[1]s\n", *model)
	cfg, err := pconfig.LoadConfigData([]byte(yaml), []llms.CallOption{llms.WithModel(*model)})
	if err != nil {
		return err
	}
	provider, err := claudecli.New(claudecli.Options{
		Name: "claude-cli-smoke", Config: cfg, DefaultModel: *model,
		ToolCallIDTemplate: "toolu_{r:24:b}", Runner: executor, CallTimeout: 3 * time.Minute,
	})
	if err != nil {
		return err
	}

	rep := &report{CLIPath: cliPath, CLIVersion: *version, Model: *model,
		AuthMethod:             "existing Claude Code subscription login of the current user (HOME passed to the CLI; not read by this program)",
		CredentialsMtimeBefore: credentialsMtime(home)}
	defer func() {
		rep.CredentialsMtimeAfter = credentialsMtime(home)
		data, _ := json.MarshalIndent(rep, "", "  ")
		_ = os.WriteFile(filepath.Join(*out, "results.json"), data, 0o600)
		fmt.Println(string(data))
	}()

	ctx := context.Background()
	for _, step := range smokeSteps(provider) {
		if *only != "" && step.name != *only {
			continue
		}
		if rep.TurnsUsed >= *turnBudget {
			rep.Stopped = fmt.Sprintf("turn budget of %d reached", *turnBudget)
			return nil
		}
		start := time.Now()
		rec, err := step.run(ctx)
		rec.LatencyMS = time.Since(start).Milliseconds()
		if n, ok := rec.Info["ClaudeCLINumTurns"].(int); ok {
			rep.TurnsUsed += n
		} else {
			rep.TurnsUsed++
		}
		if err != nil {
			rec.Error = err.Error()
		}
		rep.Calls = append(rep.Calls, rec)

		switch {
		case errors.Is(err, claudecli.ErrNativeToolActivity):
			rep.Stopped = "unexpected native tool activity"
			return nil
		case errors.Is(err, claudecli.ErrAuth) || looksLikeAuthFailure(err):
			rep.Stopped = "CLI authentication failed"
			return nil
		}
	}
	return nil
}

func looksLikeAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, w := range []string{"not logged in", "authentication", "unauthorized", "invalid api key", "oauth", "/login"} {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}

func record(name string, resp *llms.ContentResponse, err error) callRecord {
	rec := callRecord{Name: name, OK: err == nil}
	if resp == nil || len(resp.Choices) == 0 {
		return rec
	}
	c := resp.Choices[0]
	rec.Content, rec.Info = c.Content, c.GenerationInfo
	for _, tc := range c.ToolCalls {
		rec.ToolCalls = append(rec.ToolCalls, toolCall{ID: tc.ID, Name: tc.FunctionCall.Name, Arguments: tc.FunctionCall.Arguments})
	}
	return rec
}

func dummyTools() []llms.Tool {
	return []llms.Tool{
		{Type: "function", Function: &llms.FunctionDefinition{
			Name: "echo_text", Description: "Echo a piece of text back. Dummy tool for a connectivity test; it does nothing.",
			Parameters: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"text"},
				"properties": map[string]any{"text": map[string]any{"type": "string"}}},
		}},
		{Type: "function", Function: &llms.FunctionDefinition{
			Name: "add_numbers", Description: "Add two integers. Dummy tool for a connectivity test; it does nothing.",
			Parameters: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"a", "b"},
				"properties": map[string]any{"a": map[string]any{"type": "integer"}, "b": map[string]any{"type": "integer"}}},
		}},
	}
}

type smokeStep struct {
	name string
	run  func(context.Context) (callRecord, error)
}

func smokeSteps(p *claudecli.Provider) []smokeStep {
	return []smokeStep{
		{"plain_text", func(ctx context.Context) (callRecord, error) {
			text, err := p.Call(ctx, pconfig.OptionsTypeSimple,
				"This is a connectivity test. Reply with exactly PENTAGI-SMOKE-OK and nothing else.")
			rec := callRecord{Name: "plain_text", OK: err == nil, Content: text}
			return rec, err
		}},
		{"structured_json", func(ctx context.Context) (callRecord, error) {
			resp, err := p.CallWithExtraOptions(ctx, pconfig.OptionsTypeSimpleJSON, []llms.MessageContent{
				llms.TextParts(llms.ChatMessageTypeSystem, "You produce JSON only."),
				llms.TextParts(llms.ChatMessageTypeHuman,
					`Return a JSON object with exactly two keys: "status" set to "ok" and "answer" set to the number 42.`),
			}, nil, nil, llms.WithJSONMode())
			return record("structured_json", resp, err), err
		}},
		{"dummy_tool_call", func(ctx context.Context) (callRecord, error) {
			resp, err := p.CallWithTools(ctx, pconfig.OptionsTypePentester, []llms.MessageContent{
				llms.TextParts(llms.ChatMessageTypeSystem, "You are a test agent in a connectivity check. Propose tool calls when asked."),
				llms.TextParts(llms.ChatMessageTypeHuman,
					"Propose a call to the add_numbers tool that adds 19 and 23. Do not answer in text."),
			}, dummyTools(), nil)
			return record("dummy_tool_call", resp, err), err
		}},
	}
}
