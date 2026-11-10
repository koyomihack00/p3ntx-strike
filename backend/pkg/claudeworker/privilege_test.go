package claudeworker

import (
	"errors"
	"strings"
	"testing"
)

func TestPrivilege_CheckNotRoot(t *testing.T) {
	if err := CheckNotRoot(0); !errors.Is(err, ErrRoot) {
		t.Errorf("euid 0: %v", err)
	}
	if err := CheckNotRoot(1004); err != nil {
		t.Errorf("euid 1004: %v", err)
	}
}

func TestPrivilege_CheckEnvironment_NamesButNeverEchoesValues(t *testing.T) {
	err := CheckEnvironment(envMap(map[string]string{
		"ANTHROPIC_API_KEY":       "sk-ant-api-value",
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"HOME":                    "/home/worker",
	}))
	if err == nil {
		t.Fatal("forbidden variables were accepted")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || !strings.Contains(err.Error(), "CLAUDE_CODE_USE_BEDROCK") {
		t.Errorf("error does not name the variables: %v", err)
	}
	if strings.Contains(err.Error(), "sk-ant-api-value") {
		t.Error("error echoes a secret value")
	}
	if err := CheckEnvironment(envMap(map[string]string{"HOME": "/home/worker"})); err != nil {
		t.Errorf("clean environment refused: %v", err)
	}
}
