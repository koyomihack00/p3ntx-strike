// Package claudeworker runs Claude Code in headless mode for the claudecli provider
// adapter: one stateless, tool-less CLI process per request, in its own process
// group, with an environment built from scratch and a throwaway home directory.
//
// The worker must run as a dedicated unprivileged user and never inside the
// PentAGI container, which holds the Docker socket.
package claudeworker

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	// CLIPath is the absolute path of the pinned claude binary.
	CLIPath string
	// CLIVersion is reported in every result; it is configuration, not probed, so
	// the worker never runs the CLI outside a request.
	CLIVersion string
	// WorkRoot holds one private directory per call, removed when the call ends.
	// It should be a tmpfs owned by the worker user.
	WorkRoot string
	// OAuthTokenFile, when set, is read on every call and passed to the CLI as
	// CLAUDE_CODE_OAUTH_TOKEN. Its content never reaches a log or an error.
	OAuthTokenFile string
	// UseOwnerLogin runs the CLI with the worker user's real HOME so it uses that
	// user's existing Claude login. The worker never reads the login itself; the
	// CLI's --restricted flag keeps that user's settings, hooks and plugins out.
	// It excludes OAuthTokenFile.
	UseOwnerLogin bool
	// CaptureDir, when set, receives a copy of each call's raw stream-json output
	// (model output only, mode 0600) for diagnostics.
	CaptureDir string
	// HTTPSProxy, when set, is the CLI's only route out: an allowlisting proxy on a
	// loopback address, given to the CLI as HTTPS_PROXY.
	HTTPSProxy    string
	MaxConcurrent int
	// MaxTurns, when positive, caps the model turns of one CLI call (--max-turns).
	MaxTurns int
	// MaxTimeout caps the per-request timeout the client asks for.
	MaxTimeout time.Duration
	// KillGrace is how long the process group gets between SIGTERM and SIGKILL,
	// and how long output may keep arriving after the CLI itself exited.
	KillGrace time.Duration
	// MaxOutputBytes bounds the CLI's stdout; past it the call is killed.
	MaxOutputBytes int64
}

const (
	DefaultMaxConcurrent  = 2
	DefaultMaxTimeout     = 10 * time.Minute
	DefaultKillGrace      = 3 * time.Second
	DefaultMaxOutputBytes = 8 << 20
)

func (c *Config) applyDefaults() {
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = DefaultMaxConcurrent
	}
	if c.MaxTimeout <= 0 {
		c.MaxTimeout = DefaultMaxTimeout
	}
	if c.KillGrace <= 0 {
		c.KillGrace = DefaultKillGrace
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = DefaultMaxOutputBytes
	}
}

func (c *Config) validate() error {
	if !filepath.IsAbs(c.CLIPath) {
		return fmt.Errorf("CLI path %q is not absolute", c.CLIPath)
	}
	info, err := os.Stat(c.CLIPath)
	if err != nil {
		return fmt.Errorf("CLI path: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("CLI path %q is not an executable file", c.CLIPath)
	}

	if !filepath.IsAbs(c.WorkRoot) {
		return fmt.Errorf("work root %q is not absolute", c.WorkRoot)
	}
	info, err = os.Stat(c.WorkRoot)
	if err != nil {
		return fmt.Errorf("work root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("work root %q is not a directory", c.WorkRoot)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("work root must not be accessible to group or others")
	}

	if c.UseOwnerLogin && c.OAuthTokenFile != "" {
		return errors.New("use either the owner's login or an OAuth token file, not both")
	}
	if c.CaptureDir != "" {
		info, err := os.Stat(c.CaptureDir)
		if err != nil || !filepath.IsAbs(c.CaptureDir) || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("capture dir must be an absolute directory private to the worker user")
		}
	}
	if c.HTTPSProxy != "" {
		u, err := url.Parse(c.HTTPSProxy)
		if err != nil || u.Scheme != "http" || u.Port() == "" || u.User != nil || (u.Path != "" && u.Path != "/") {
			return errors.New("HTTPS proxy must be http://LOOPBACK-IP:PORT")
		}
		addr, err := netip.ParseAddr(u.Hostname())
		if err != nil || !addr.IsLoopback() {
			return errors.New("HTTPS proxy must listen on a loopback address")
		}
	}
	if c.OAuthTokenFile != "" {
		if !filepath.IsAbs(c.OAuthTokenFile) {
			return errors.New("OAuth token file path is not absolute")
		}
		if _, err := ReadSecretFile(c.OAuthTokenFile); err != nil {
			return err
		}
	}
	return nil
}
