package claudeworker

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestStream_ParseStream_CollectsEvidenceUpToTheResult(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[{"name":"local"}]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"x"},{"type":"tool_use","name":"StructuredOutput"}]}}`,
		`{"type":"user","message":{"content":"plain string content"}}`,
		`{"type":"assistant","message":{"content":[{"type":"mcp_tool_use","name":"mcp__x__y"},{"type":"server_tool_use"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","structured_output":{"a":1},` +
			`"errors":["e"],"stop_reason":"end_turn","num_turns":3,"total_cost_usd":0.5,"permission_denials":[{},{}],` +
			`"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`,
		`this line after the result is never read`,
	}, "\n")

	res, err := parseStream(strings.NewReader(stream), 1<<20)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if !slices.Equal(res.InitTools, []string{"StructuredOutput", "mcp_server:local"}) {
		t.Errorf("init tools = %q", res.InitTools)
	}
	if !slices.Equal(res.ToolUses, []string{"StructuredOutput", "mcp__x__y", "server_tool_use"}) {
		t.Errorf("tool uses = %q", res.ToolUses)
	}
	if res.Subtype != "success" || res.Text != "done" || string(res.StructuredOutput) != `{"a":1}` ||
		res.StopReason != "end_turn" || res.NumTurns != 3 || res.TotalCostUSD != 0.5 || res.PermissionDenials != 2 ||
		!slices.Equal(res.Errors, []string{"e"}) {
		t.Errorf("result = %+v", res)
	}
	if res.Usage.InputTokens != 1 || res.Usage.OutputTokens != 2 || res.Usage.CacheReadInputTokens != 3 ||
		res.Usage.CacheCreationInputTokens != 4 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestStream_ParseStream_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		limit  int64
		want   error
	}{
		{"not json", "hello\n", 1 << 20, errMalformedStream},
		{"event without type", `{"subtype":"init"}`, 1 << 20, errMalformedStream},
		{"assistant content of the wrong shape", `{"type":"assistant","message":{"content":42}}`, 1 << 20, errMalformedStream},
		{"truncated event", `{"type":"result","subtype":`, 1 << 20, errMalformedStream},
		{"over the limit", `{"type":"system","subtype":"noise","pad":"` + strings.Repeat("x", 4096) + `"}`, 1024, errOutputTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := parseStream(strings.NewReader(tc.stream), tc.limit)
			if !errors.Is(err, tc.want) || res != nil {
				t.Errorf("res=%v err=%v, want %v", res, err, tc.want)
			}
		})
	}
}

func TestStream_ParseStream_EndWithoutResultIsNoResult(t *testing.T) {
	res, err := parseStream(strings.NewReader(`{"type":"system","subtype":"init","tools":[]}`+"\n"), 1<<20)
	if res != nil || err != nil {
		t.Errorf("res=%v err=%v, want neither", res, err)
	}
}
