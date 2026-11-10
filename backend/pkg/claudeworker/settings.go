package claudeworker

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Settings is everything the worker binary reads from its environment. Secrets are
// named by file path, never passed as values.
type Settings struct {
	Config    Config
	Listen    string
	TokenFile string
}

func SettingsFromEnv(lookup func(string) (string, bool)) (Settings, error) {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	s := Settings{
		Config: Config{
			CLIPath:        get("CLAUDE_WORKER_CLI"),
			CLIVersion:     get("CLAUDE_WORKER_CLI_VERSION"),
			WorkRoot:       get("CLAUDE_WORKER_WORK_ROOT"),
			OAuthTokenFile: get("CLAUDE_WORKER_OAUTH_TOKEN_FILE"),
			HTTPSProxy:     get("CLAUDE_WORKER_HTTPS_PROXY"),
			UseOwnerLogin:  get("CLAUDE_WORKER_USE_OWNER_LOGIN") == "1",
			CaptureDir:     get("CLAUDE_WORKER_CAPTURE_DIR"),
		},
		Listen:    get("CLAUDE_WORKER_LISTEN"),
		TokenFile: get("CLAUDE_WORKER_TOKEN_FILE"),
	}
	if s.Listen == "" || s.TokenFile == "" || s.Config.CLIPath == "" || s.Config.WorkRoot == "" {
		return s, errors.New("CLAUDE_WORKER_LISTEN, CLAUDE_WORKER_TOKEN_FILE, CLAUDE_WORKER_CLI and CLAUDE_WORKER_WORK_ROOT are required")
	}
	if v := get("CLAUDE_WORKER_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return s, errors.New("CLAUDE_WORKER_MAX_CONCURRENT must be a positive integer")
		}
		s.Config.MaxConcurrent = n
	}
	if v := get("CLAUDE_WORKER_MAX_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return s, errors.New("CLAUDE_WORKER_MAX_TIMEOUT must be a positive duration")
		}
		s.Config.MaxTimeout = d
	}
	return s, nil
}

// ReadSecretFile refuses a file that group or others can read.
func ReadSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("secret file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret file %s must not be accessible to group or others", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("secret file: %w", err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	return secret, nil
}

// Listen accepts "unix:/path" or "tcp:IP:port". A TCP address must be a literal,
// specific IP: binding every interface would expose the worker beyond the host.
func Listen(spec string) (net.Listener, error) {
	network, addr, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("listen spec %q is not unix:PATH or tcp:IP:PORT", spec)
	}
	switch network {
	case "unix":
		if info, err := os.Lstat(addr); err == nil && info.Mode()&os.ModeSocket != 0 {
			if err := os.Remove(addr); err != nil {
				return nil, fmt.Errorf("remove stale socket: %w", err)
			}
		}
		ln, err := net.Listen("unix", addr)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(addr, 0o660); err != nil {
			ln.Close()
			return nil, err
		}
		return ln, nil
	case "tcp":
		ap, err := netip.ParseAddrPort(addr)
		if err != nil {
			return nil, fmt.Errorf("tcp listen address %q is not IP:PORT", addr)
		}
		if ap.Addr().IsUnspecified() {
			return nil, errors.New("refusing to listen on an unspecified address")
		}
		return net.Listen("tcp", ap.String())
	default:
		return nil, fmt.Errorf("unsupported listen network %q", network)
	}
}
