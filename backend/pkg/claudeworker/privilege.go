package claudeworker

import (
	"errors"
	"fmt"
	"strings"
)

// forbiddenEnv would make the CLI bill an API account or a cloud provider instead
// of the configured credential: in print mode ANTHROPIC_API_KEY takes precedence
// over OAuth. The child environment never inherits them, but their presence in the
// worker's own environment signals a misconfiguration worth refusing to start on.
var forbiddenEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
}

var ErrRoot = errors.New("claude worker refuses to run as root")

func CheckNotRoot(euid int) error {
	if euid == 0 {
		return ErrRoot
	}
	return nil
}

// CheckEnvironment reports forbidden variables by name only, never by value.
func CheckEnvironment(lookup func(string) (string, bool)) error {
	var found []string
	for _, name := range forbiddenEnv {
		if _, ok := lookup(name); ok {
			found = append(found, name)
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("claude worker refuses to start with %s set", strings.Join(found, ", "))
	}
	return nil
}
