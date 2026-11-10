package claudeworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"pentagi/pkg/claudeworker/protocol"
)

var (
	errMalformedStream = errors.New("malformed CLI output")
	errOutputTooLarge  = errors.New("CLI output exceeded the limit")
)

type streamEvent struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	Tools      []string        `json:"tools"`
	MCPServers []mcpServer     `json:"mcp_servers"`
	Message    json.RawMessage `json:"message"`

	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	StructuredOutput  json.RawMessage   `json:"structured_output"`
	Errors            []string          `json:"errors"`
	StopReason        string            `json:"stop_reason"`
	NumTurns          int               `json:"num_turns"`
	Usage             protocol.Usage    `json:"usage"`
	TotalCostUSD      float64           `json:"total_cost_usd"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
}

type mcpServer struct {
	Name string `json:"name"`
}

type contentBlock struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n >= c.limit {
		return 0, errOutputTooLarge
	}
	if remaining := c.limit - c.n; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// parseStream reads stream-json events up to the result message and returns it
// with the tool evidence seen on the way: the init event's tools and MCP servers,
// and every tool-use block of any kind. Anything it cannot parse is an error: the
// worker never guesses at output it does not understand.
func parseStream(r io.Reader, limit int64) (*protocol.Result, error) {
	dec := json.NewDecoder(&countingReader{r: r, limit: limit})
	evidence := &protocol.Result{}

	for {
		var ev streamEvent
		if err := dec.Decode(&ev); err != nil {
			switch {
			case errors.Is(err, errOutputTooLarge):
				return nil, errOutputTooLarge
			case errors.Is(err, io.EOF):
				return nil, nil
			default:
				return nil, fmt.Errorf("%w: %v", errMalformedStream, err)
			}
		}

		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				evidence.InitTools = append(evidence.InitTools, ev.Tools...)
				for _, server := range ev.MCPServers {
					evidence.InitTools = append(evidence.InitTools, "mcp_server:"+server.Name)
				}
			}
		case "assistant":
			names, err := toolUses(ev.Message)
			if err != nil {
				return nil, err
			}
			evidence.ToolUses = append(evidence.ToolUses, names...)
		case "result":
			evidence.Subtype = ev.Subtype
			evidence.IsError = ev.IsError
			evidence.Text = ev.Result
			evidence.StructuredOutput = ev.StructuredOutput
			evidence.Errors = ev.Errors
			evidence.StopReason = ev.StopReason
			evidence.NumTurns = ev.NumTurns
			evidence.Usage = ev.Usage
			evidence.TotalCostUSD = ev.TotalCostUSD
			evidence.PermissionDenials = len(ev.PermissionDenials)
			return evidence, nil
		case "":
			return nil, fmt.Errorf("%w: event without a type", errMalformedStream)
		}
	}
}

// toolUses names every block whose type ends in "tool_use": client tools,
// server-side tools and MCP tools all count as tool activity.
func toolUses(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("%w: assistant message: %v", errMalformedStream, err)
	}
	var blocks []contentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		var text string
		if json.Unmarshal(msg.Content, &text) == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: assistant content: %v", errMalformedStream, err)
	}
	var names []string
	for _, b := range blocks {
		if strings.HasSuffix(b.Type, "tool_use") {
			name := b.Name
			if name == "" {
				name = b.Type
			}
			names = append(names, name)
		}
	}
	return names, nil
}
