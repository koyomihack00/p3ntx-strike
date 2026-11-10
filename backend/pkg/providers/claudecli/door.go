package claudecli

import (
	"embed"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"pentagi/pkg/config"
	"pentagi/pkg/providers/anthropic"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/system"

	"github.com/vxcontrol/langchaingo/llms"
)

//go:embed config.yml
var configFS embed.FS

// DefaultModel is Claude Code's own alias; the CLI resolves it to the current model.
const DefaultModel = "sonnet"

// unixWorkerHost is a placeholder: over a Unix socket the URL host is never dialled.
const unixWorkerHost = "http://claude-worker"

func BuildProviderConfig(configData []byte) (*pconfig.ProviderConfig, error) {
	return pconfig.LoadConfigData(configData, []llms.CallOption{
		llms.WithModel(DefaultModel),
		llms.WithN(1),
	})
}

func DefaultProviderConfig() (*pconfig.ProviderConfig, error) {
	configData, err := configFS.ReadFile("config.yml")
	if err != nil {
		return nil, err
	}
	return BuildProviderConfig(configData)
}

// Configured reports whether PentAGI's configuration names a worker; the provider
// stays disabled otherwise.
func Configured(cfg *config.Config) bool {
	return cfg.ClaudeCLIWorkerURL != "" && cfg.ClaudeCLIWorkerToken != ""
}

// NewFromConfig builds the provider that reaches cmd/claudeworker at
// CLAUDE_CLI_WORKER_URL with CLAUDE_CLI_WORKER_TOKEN.
func NewFromConfig(
	cfg *config.Config,
	providerName provider.ProviderName,
	providerConfig *pconfig.ProviderConfig,
) (provider.Provider, error) {
	runner, err := runnerFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	models, err := anthropic.DefaultModels()
	if err != nil {
		return nil, err
	}
	return New(Options{
		Name:               providerName,
		Config:             providerConfig,
		Models:             models,
		DefaultModel:       DefaultModel,
		ToolCallIDTemplate: anthropic.AnthropicToolCallIDTemplate,
		Runner:             runner,
		CallTimeout:        time.Duration(cfg.ClaudeCLICallTimeout) * time.Second,
	})
}

func runnerFromConfig(cfg *config.Config) (*HTTPRunner, error) {
	if !Configured(cfg) {
		return nil, errors.New("CLAUDE_CLI_WORKER_URL and CLAUDE_CLI_WORKER_TOKEN are required for the Claude CLI provider")
	}
	base, err := system.GetHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	// The shared client's timeout would cut a long CLI call; each call is bounded
	// by its context and CallTimeout instead.
	client := *base
	client.Timeout = 0

	if path, ok := strings.CutPrefix(cfg.ClaudeCLIWorkerURL, "unix://"); ok {
		if !filepath.IsAbs(path) {
			return nil, errors.New("CLAUDE_CLI_WORKER_URL must name an absolute socket path: unix:///path/to/worker.sock")
		}
		client.Transport = UnixSocketTransport(path)
		return NewHTTPRunner(unixWorkerHost, cfg.ClaudeCLIWorkerToken, &client)
	}
	return NewHTTPRunner(cfg.ClaudeCLIWorkerURL, cfg.ClaudeCLIWorkerToken, &client)
}
