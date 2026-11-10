package claudeworker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfig_Validate(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	notExec := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExec, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	openWork := filepath.Join(dir, "open")
	if err := os.Mkdir(openWork, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(openWork, 0o755); err != nil {
		t.Fatal(err)
	}
	sharedToken := filepath.Join(dir, "token")
	if err := os.WriteFile(sharedToken, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sharedToken, 0o644); err != nil {
		t.Fatal(err)
	}

	valid := Config{CLIPath: cli, WorkRoot: work}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	for _, proxy := range []string{"http://127.0.0.1:3128", "http://[::1]:3128/"} {
		c := valid
		c.HTTPSProxy = proxy
		if err := c.validate(); err != nil {
			t.Errorf("proxy %s refused: %v", proxy, err)
		}
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"relative CLI", func(c *Config) { c.CLIPath = "claude" }},
		{"missing CLI", func(c *Config) { c.CLIPath = filepath.Join(dir, "nope") }},
		{"CLI not executable", func(c *Config) { c.CLIPath = notExec }},
		{"relative work root", func(c *Config) { c.WorkRoot = "work" }},
		{"work root readable by others", func(c *Config) { c.WorkRoot = openWork }},
		{"work root is a file", func(c *Config) { c.WorkRoot = cli }},
		{"relative token file", func(c *Config) { c.OAuthTokenFile = "token" }},
		{"token file readable by others", func(c *Config) { c.OAuthTokenFile = sharedToken }},
		{"proxy on a public address", func(c *Config) { c.HTTPSProxy = "http://203.0.113.5:3128" }},
		{"proxy by hostname", func(c *Config) { c.HTTPSProxy = "http://localhost:3128" }},
		{"proxy without port", func(c *Config) { c.HTTPSProxy = "http://127.0.0.1" }},
		{"proxy with credentials", func(c *Config) { c.HTTPSProxy = "http://u:p@127.0.0.1:3128" }},
		{"socks proxy", func(c *Config) { c.HTTPSProxy = "socks5://127.0.0.1:1080" }},
		{"owner login and token file", func(c *Config) { c.UseOwnerLogin, c.OAuthTokenFile = true, sharedToken }},
		{"relative capture dir", func(c *Config) { c.CaptureDir = "capture" }},
		{"capture dir readable by others", func(c *Config) { c.CaptureDir = openWork }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			tc.mutate(&c)
			if err := c.validate(); err == nil {
				t.Error("accepted")
			}
		})
	}
}
