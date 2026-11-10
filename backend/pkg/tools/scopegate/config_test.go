package scopegate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"pentagi/pkg/config"
	"pentagi/pkg/database"
	"pentagi/pkg/tools"
)

func labConfig() *config.Config {
	return &config.Config{
		ScopeGateCIDRs:          []string{"10.10.10.0/24", " 192.168.56.101 ", ""},
		ScopeGateHosts:          []string{"target.lab", "*.corp.lab"},
		ScopeGateExternalSearch: "deny",
	}
}

func gateFromConfig(t *testing.T, cfg *config.Config) *Gate {
	t.Helper()
	g, err := FromConfig(cfg)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	return g
}

func toolCall(agent database.MsgchainType, tool string, args map[string]any) tools.ToolCall {
	raw, _ := json.Marshal(args)
	return tools.ToolCall{FlowID: 7, Agent: agent, Tool: tool, Args: raw}
}

func terminalCall(cmd string) tools.ToolCall {
	return toolCall("pentester", "terminal", map[string]any{"input": cmd, "cwd": "/work", "message": "m"})
}

func browserCall(url string) tools.ToolCall {
	return toolCall("pentester", "browser", map[string]any{"url": url, "action": "markdown", "message": "m"})
}

// refusal is "" when the call may run, otherwise the error text, which must wrap ErrDenied.
func refusal(t *testing.T, g *Gate, call tools.ToolCall) string {
	t.Helper()
	err := g.AuthorizeToolCall(t.Context(), call)
	if err == nil {
		return ""
	}
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("a refusal must wrap ErrDenied, got: %v", err)
	}
	return err.Error()
}

func TestConfig_FromConfig_RejectsAnEntryItCannotParse(t *testing.T) {
	cases := map[string]func(*config.Config){
		"a CIDR with a bad prefix length": func(c *config.Config) { c.ScopeGateCIDRs = []string{"10.0.0.0/33"} },
		"an address that is not one":      func(c *config.Config) { c.ScopeGateCIDRs = []string{"10.0.0.256"} },
		"a hostname as a CIDR":            func(c *config.Config) { c.ScopeGateCIDRs = []string{"target.lab"} },
		"a host with a scheme":            func(c *config.Config) { c.ScopeGateHosts = []string{"https://target.lab"} },
		"a host with a path":              func(c *config.Config) { c.ScopeGateHosts = []string{"target.lab/admin"} },
		"a bare wildcard":                 func(c *config.Config) { c.ScopeGateHosts = []string{"*"} },
		"a wildcard inside a name":        func(c *config.Config) { c.ScopeGateHosts = []string{"app.*.lab"} },
		"an unknown search verdict":       func(c *config.Config) { c.ScopeGateExternalSearch = "require_approval" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := labConfig()
			mutate(cfg)
			if g, err := FromConfig(cfg); err == nil || g != nil {
				t.Fatalf("an unparseable entry must stop the gate from being built, got gate %v, error %v", g, err)
			}
		})
	}
}

func TestConfig_FromConfig_EnforcesTheConfiguredScope(t *testing.T) {
	g := gateFromConfig(t, labConfig())

	cases := []struct {
		name   string
		call   tools.ToolCall
		refuse string // "" allows; otherwise a fragment of the reason
	}{
		{"browser to an in-scope address", browserCall("http://10.10.10.5/admin"), ""},
		{"browser to a padded single address", browserCall("http://192.168.56.101/"), ""},
		{"browser to an in-scope host", browserCall("https://target.lab/login"), ""},
		{"browser to a scoped subdomain", browserCall("https://app.corp.lab/"), ""},
		{"browser to an out-of-scope address", browserCall("http://10.10.11.5/"), "out of scope"},
		{"browser to an out-of-scope host", browserCall("https://evil.example.com/"), "out of scope"},
		{"browser to the apex of a wildcard", browserCall("https://corp.lab/"), "out of scope"},
		{"browser to cloud metadata", browserCall("http://169.254.169.254/latest/meta-data/"), "out of scope"},
		{"browser to a file URL", browserCall("file:///etc/passwd"), "not http or https"},

		{"scanner against an in-scope target", terminalCall("nmap -sV 10.10.10.5"), "approval required but no approver"},
		{"client against an in-scope host", terminalCall("curl https://target.lab/"), "approval required but no approver"},
		{"scanner against an out-of-scope target", terminalCall("nmap -sV 10.10.20.5"), "out of scope"},
		{"one in-scope and one out-of-scope target", terminalCall("nmap 10.10.10.5,8.8.8.8"), "out of scope"},
		{"a local command chained to an out-of-scope one", terminalCall("ls; curl http://8.8.8.8/"), "out of scope"},

		{"listing the work directory", terminalCall("ls -la /work"), ""},
		{"filtering a saved scan", terminalCall("cat /work/scan.gnmap | grep open | wc -l"), ""},
		{"writing a local file", terminalCall("echo done > /work/status.txt"), ""},
		{"printing a URL is not fetching it", terminalCall("echo see http://evil.example.com"), ""},

		{"a result tool", toolCall("pentester", "hack_result", map[string]any{"result": "r", "message": "m"}), ""},
		{"delegation to another agent", toolCall("primary_agent", "pentester", map[string]any{"question": "q", "message": "m"}), ""},
		{"the file tool", toolCall("coder", "file", map[string]any{"action": "read_file", "path": "/work/a", "message": "m"}), ""},
		{"web search while external search is denied", toolCall("searcher", "web_search", map[string]any{"query": "CVE-2021-44228", "message": "m"}), "external search"},

		{"an executor with no agent", toolCall("", "terminal", map[string]any{"input": "id", "message": "m"}), "not allowed for agent"},
		{"an agent that owns no executor", toolCall("summarizer", "terminal", map[string]any{"input": "id", "message": "m"}), "not allowed for agent"},
		{"an operator-defined function", toolCall("pentester", "custom_fn", map[string]any{"url": "http://8.8.8.8/"}), "not allowed for agent"},
		{"terminal arguments that do not decode", tools.ToolCall{Agent: "pentester", Tool: "terminal", Args: json.RawMessage(`{"input":7}`)}, "unparseable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := refusal(t, g, tc.call)
			switch {
			case tc.refuse == "" && got != "":
				t.Errorf("refused: %s", got)
			case tc.refuse != "" && !strings.Contains(got, tc.refuse):
				t.Errorf("want a refusal mentioning %q, got %q", tc.refuse, got)
			}
		})
	}
}

func TestConfig_FromConfig_EmptyScopeRefusesEveryTarget(t *testing.T) {
	g := gateFromConfig(t, &config.Config{})

	for _, call := range []tools.ToolCall{
		browserCall("http://10.10.10.5/"),
		browserCall("https://target.lab/"),
		terminalCall("curl http://10.10.10.5/"),
		terminalCall("ping -c1 8.8.8.8"),
		toolCall("searcher", "web_search", map[string]any{"query": "q", "message": "m"}),
	} {
		if refusal(t, g, call) == "" {
			t.Errorf("with no scope configured, %s %s ran", call.Tool, call.Args)
		}
	}
	if got := refusal(t, g, terminalCall("ls /work")); got != "" {
		t.Errorf("a local command that names no target must still run with an empty scope: %s", got)
	}
}

func TestConfig_FromConfig_RefusesShellBypassesEvenForInScopeTargets(t *testing.T) {
	g := gateFromConfig(t, labConfig())

	for _, cmd := range []string{
		"sh -c 'curl http://10.10.10.5/'",
		"bash -c 'nmap 10.10.10.5'",
		"/bin/sh -c id",
		"echo nmap 10.10.10.5 | sh",
		"echo bm1hcCAxMC4xMC4xMC41 | base64 -d | sh",
		"$(echo curl) http://10.10.10.5/",
		"`echo curl` http://10.10.10.5/",
		"T=10.10.10.5; curl $T",
		"env curl http://10.10.10.5/",
		"xargs curl < /work/urls.txt",
		"find /work -exec curl http://10.10.10.5/ ;",
		"awk 'BEGIN{system(\"curl http://10.10.10.5/\")}'",
		"python3 -c 'import urllib.request'",
		"perl -e 'print 1'",
		"busybox wget http://10.10.10.5/",
		"cat < /dev/tcp/10.10.10.5/80",
		"nohup nmap 10.10.10.5 &",
		"timeout 5 curl http://10.10.10.5/",
		"ls\ncurl http://10.10.10.5/",
		"/tmp/ls",
		"PATH=/tmp ls",
		"./scan.sh",
		"",
	} {
		t.Run(cmd, func(t *testing.T) {
			if refusal(t, g, terminalCall(cmd)) == "" {
				t.Errorf("%q ran without approval", cmd)
			}
		})
	}
}

func TestConfig_FromConfig_ExternalSearchFollowsTheSetting(t *testing.T) {
	search := toolCall("searcher", "web_search", map[string]any{"query": "CVE-2021-44228", "message": "m"})

	for setting, wantRun := range map[string]bool{"": false, "deny": false, "allow": true, " ALLOW ": true} {
		cfg := labConfig()
		cfg.ScopeGateExternalSearch = setting
		if ran := refusal(t, gateFromConfig(t, cfg), search) == ""; ran != wantRun {
			t.Errorf("SCOPE_GATE_EXTERNAL_SEARCH=%q: web_search ran = %v, want %v", setting, ran, wantRun)
		}
	}
}

func TestConfig_FromConfig_EveryExecutorAgentMayRunALocalCommand(t *testing.T) {
	g := gateFromConfig(t, labConfig())

	for _, agent := range []database.MsgchainType{
		"assistant", "primary_agent", "installer", "coder", "pentester", "searcher",
		"generator", "refiner", "memorist", "enricher", "reporter",
	} {
		call := toolCall(agent, "terminal", map[string]any{"input": "id", "message": "m"})
		if got := refusal(t, g, call); got != "" {
			t.Errorf("agent %s cannot run a local command: %s", agent, got)
		}
	}
}
