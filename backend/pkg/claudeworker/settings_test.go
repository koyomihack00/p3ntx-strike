package claudeworker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestSettings_SettingsFromEnv(t *testing.T) {
	base := map[string]string{
		"CLAUDE_WORKER_LISTEN":     "unix:/run/claude-worker/worker.sock",
		"CLAUDE_WORKER_TOKEN_FILE": "/etc/claude-worker/token",
		"CLAUDE_WORKER_CLI":        "/opt/claude/bin/claude",
		"CLAUDE_WORKER_WORK_ROOT":  "/run/claude-worker/calls",
	}
	s, err := SettingsFromEnv(envMap(base))
	if err != nil || s.Config.CLIPath != "/opt/claude/bin/claude" || s.Listen != "unix:/run/claude-worker/worker.sock" {
		t.Fatalf("settings %+v, %v", s, err)
	}

	withExtra := map[string]string{"CLAUDE_WORKER_MAX_CONCURRENT": "3", "CLAUDE_WORKER_MAX_TIMEOUT": "90s",
		"CLAUDE_WORKER_HTTPS_PROXY": "http://127.0.0.1:3128"}
	for k, v := range base {
		withExtra[k] = v
	}
	s, err = SettingsFromEnv(envMap(withExtra))
	if err != nil || s.Config.MaxConcurrent != 3 || s.Config.MaxTimeout != 90*time.Second ||
		s.Config.HTTPSProxy != "http://127.0.0.1:3128" {
		t.Errorf("settings %+v, %v", s.Config, err)
	}

	bad := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"no listen address", func(m map[string]string) { delete(m, "CLAUDE_WORKER_LISTEN") }},
		{"no token file", func(m map[string]string) { delete(m, "CLAUDE_WORKER_TOKEN_FILE") }},
		{"no CLI", func(m map[string]string) { delete(m, "CLAUDE_WORKER_CLI") }},
		{"no work root", func(m map[string]string) { delete(m, "CLAUDE_WORKER_WORK_ROOT") }},
		{"zero concurrency", func(m map[string]string) { m["CLAUDE_WORKER_MAX_CONCURRENT"] = "0" }},
		{"unparseable timeout", func(m map[string]string) { m["CLAUDE_WORKER_MAX_TIMEOUT"] = "soon" }},
	}
	for _, tc := range bad {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		tc.mutate(m)
		if _, err := SettingsFromEnv(envMap(m)); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestSettings_ReadSecretFile_RefusesSharedFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if got, err := ReadSecretFile(write("ok", " s3cret \n", 0o600)); err != nil || got != "s3cret" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := ReadSecretFile(write("shared", "s3cret", 0o644)); err == nil {
		t.Error("a world-readable secret was accepted")
	}
	if _, err := ReadSecretFile(write("empty", "\n", 0o600)); err == nil {
		t.Error("an empty secret was accepted")
	}
	if _, err := ReadSecretFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing secret was accepted")
	}
}

func TestSettings_Listen(t *testing.T) {
	for _, bad := range []string{"tcp:0.0.0.0:8765", "tcp:[::]:8765", "tcp:localhost:8765", "udp:127.0.0.1:1", "127.0.0.1:8765"} {
		if ln, err := Listen(bad); err == nil {
			ln.Close()
			t.Errorf("%q was accepted", bad)
		}
	}

	ln, err := Listen("tcp:127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback: %v", err)
	}
	ln.Close()

	sock := filepath.Join(t.TempDir(), "w.sock")
	for range 2 {
		ln, err := Listen("unix:" + sock)
		if err != nil {
			t.Fatalf("unix (stale socket must be replaced): %v", err)
		}
		info, err := os.Stat(sock)
		if err != nil || info.Mode().Perm() != 0o660 {
			t.Errorf("socket mode %v, %v", info.Mode().Perm(), err)
		}
		ln.(interface{ SetUnlinkOnClose(bool) }).SetUnlinkOnClose(false)
		ln.Close()
	}
}
