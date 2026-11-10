package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"pentagi/pkg/providers/pconfig"
)

func TestDecision_Parse_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{"no structured output", ``},
		{"not json", `{"kind":"tool_calls",`},
		{"string instead of object", `"run id"`},
		{"unknown kind", `{"kind":"shell","content":"id"}`},
		{"empty text", `{"kind":"text","content":""}`},
		{"text plus calls", `{"kind":"text","content":"x","calls":[]}`},
		{"calls plus content", `{"kind":"tool_calls","content":"x","calls":[{"name":"terminal","arguments":` +
			`{"input":"id","cwd":"/work","detach":false,"timeout":0,"message":"m"}}]}`},
		{"text without content", `{"kind":"text"}`},
		{"tool_calls without calls", `{"kind":"tool_calls"}`},
		{"no calls", `{"kind":"tool_calls","calls":[]}`},
		{"unknown tool", `{"kind":"tool_calls","calls":[{"name":"bash","arguments":{"command":"id"}}]}`},
		{"tool not offered this turn", `{"kind":"tool_calls","calls":[{"name":"browser","arguments":` +
			`{"url":"http://10.0.0.1","action":"markdown","message":"m"}}]}`},
		{"missing required argument", `{"kind":"tool_calls","calls":[{"name":"terminal","arguments":` +
			`{"input":"id","cwd":"/work","detach":false,"timeout":0}}]}`},
		{"wrong argument type", `{"kind":"tool_calls","calls":[{"name":"terminal","arguments":` +
			`{"input":"id","cwd":"/work","detach":"yes","timeout":0,"message":"m"}}]}`},
		{"arguments as a string", `{"kind":"tool_calls","calls":[{"name":"terminal","arguments":` +
			`"{\"input\":\"id\"}"}]}`},
		{"model-supplied id field", `{"kind":"tool_calls","calls":[{"id":"toolu_x","name":"terminal","arguments":` +
			`{"input":"id","cwd":"/work","detach":false,"timeout":0,"message":"m"}}]}`},
		{"too many calls", manyTerminalCalls(9)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{reply: replyWith(structured(tc.output))}
			p := newTestProvider(t, runner)

			resp, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
				pentagiTools(t, "terminal", "hack_result"), nil)
			if !errors.Is(err, ErrInvalidDecision) {
				t.Fatalf("err = %v, want ErrInvalidDecision", err)
			}
			if resp != nil {
				t.Errorf("a response escaped: %+v", resp)
			}
			if runner.calls() != 1 {
				t.Errorf("runner called %d times; an invalid decision is retried by the agent loop, not here", runner.calls())
			}
		})
	}
}

func TestDecision_Parse_AcceptsTheMaximumCallsWithDistinctIDs(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{reply: replyWith(structured(manyTerminalCalls(8)))},
		func(o *Options) { o.ToolCallIDTemplate = "call_{r:1:d}" })

	resp, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
		pentagiTools(t, "terminal"), nil)
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	seen := map[string]bool{}
	for _, call := range resp.Choices[0].ToolCalls {
		if seen[call.ID] {
			t.Fatalf("duplicate ID %q among %d calls", call.ID, len(resp.Choices[0].ToolCalls))
		}
		seen[call.ID] = true
	}
	if len(seen) != 8 {
		t.Errorf("got %d calls, want 8", len(seen))
	}
}

// The CLI passes the schema to the API as a tool input schema: the root must be an
// object and may not combine alternatives.
func TestDecision_BuildSpec_SchemaIsAnAPICompatibleObject(t *testing.T) {
	runner := &fakeRunner{reply: replyWith(structured(terminalCall))}
	p := newTestProvider(t, runner)

	if _, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
		pentagiTools(t, "terminal", "file", "browser", "hack_result"), nil); err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	req := runner.lastRequest(t)

	var schema map[string]any
	if err := json.Unmarshal(req.OutputSchema, &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("root type = %v, want object", schema["type"])
	}
	for _, k := range []string{"oneOf", "anyOf", "allOf", "not", "if"} {
		if _, ok := schema[k]; ok {
			t.Errorf("root carries %q", k)
		}
	}
	calls := schema["properties"].(map[string]any)["calls"].(map[string]any)
	names := calls["items"].(map[string]any)["properties"].(map[string]any)["name"].(map[string]any)["enum"]
	if got, _ := json.Marshal(names); string(got) != `["terminal","file","browser","hack_result"]` {
		t.Errorf("tool name enum = %s", got)
	}
	if calls["maxItems"] != float64(8) {
		t.Errorf("maxItems = %v, want 8", calls["maxItems"])
	}

	for _, want := range []string{"- terminal: ", "- hack_result: ", `"input"`, `"message"`} {
		if !strings.Contains(req.SystemPrompt, want) {
			t.Errorf("system prompt lacks %q from the tool catalogue", want)
		}
	}
	if strings.Contains(req.SystemPrompt, "$schema") {
		t.Error("catalogue keeps $schema")
	}
}

func TestDecision_BuildSpec_RejectsDuplicateTools(t *testing.T) {
	runner := &fakeRunner{reply: replyWith(structured(terminalCall))}
	p := newTestProvider(t, runner)

	_, err := p.CallWithTools(context.Background(), pconfig.OptionsTypePentester, taskChain(),
		pentagiTools(t, "terminal", "terminal"), nil)
	if !errors.Is(err, ErrUnsupportedRequest) || runner.calls() != 0 {
		t.Errorf("err=%v calls=%d, want ErrUnsupportedRequest before any call", err, runner.calls())
	}
}

func manyTerminalCalls(n int) string {
	calls := make([]string, n)
	for i := range calls {
		calls[i] = fmt.Sprintf(`{"name":"terminal","arguments":{"input":"echo %d","cwd":"/work",`+
			`"detach":false,"timeout":0,"message":"step %d"}}`, i, i)
	}
	return `{"kind":"tool_calls","calls":[` + strings.Join(calls, ",") + `]}`
}
