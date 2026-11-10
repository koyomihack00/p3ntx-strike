package claudecli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"pentagi/pkg/templates"

	"github.com/vxcontrol/langchaingo/llms"
	"github.com/xeipuuv/gojsonschema"
)

type replyMode int

const (
	replyText replyMode = iota
	replyJSON
	replyDecision
)

const (
	maxCallsPerDecision = 8
	maxReportedErrors   = 5
	kindText            = "text"
	kindToolCalls       = "tool_calls"
)

var jsonObjectSchema = json.RawMessage(`{"type":"object"}`)

type decisionSpec struct {
	schema json.RawMessage
	whole  *gojsonschema.Schema
	tools  map[string]*gojsonschema.Schema
	// catalog describes each offered tool and its argument schema for the system
	// prompt: the output schema only names the tools.
	catalog string
}

func compileDraft7(raw json.RawMessage) (*gojsonschema.Schema, error) {
	loader := gojsonschema.NewSchemaLoader()
	loader.Draft = gojsonschema.Draft7
	loader.AutoDetect = false
	return loader.Compile(gojsonschema.NewBytesLoader(raw))
}

// toolParameters returns a tool's argument schema without the root keywords that
// would re-scope it once embedded in the decision schema.
func toolParameters(tool llms.Tool) (json.RawMessage, error) {
	if tool.Function == nil || tool.Function.Name == "" {
		return nil, fmt.Errorf("%w: tool without a function name", ErrUnsupportedRequest)
	}
	if tool.Function.Parameters == nil {
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`), nil
	}

	raw, err := json.Marshal(tool.Function.Parameters)
	if err != nil {
		return nil, fmt.Errorf("%w: tool %q parameters: %v", ErrUnsupportedRequest, tool.Function.Name, err)
	}
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("%w: tool %q parameters are not an object schema", ErrUnsupportedRequest, tool.Function.Name)
	}
	delete(params, "$schema")
	delete(params, "$id")
	return json.Marshal(params)
}

func buildDecisionSpec(tools []llms.Tool) (*decisionSpec, error) {
	spec := &decisionSpec{tools: make(map[string]*gojsonschema.Schema, len(tools))}
	names := make([]string, 0, len(tools))
	var catalog strings.Builder

	for _, tool := range tools {
		params, err := toolParameters(tool)
		if err != nil {
			return nil, err
		}
		name := tool.Function.Name
		if _, dup := spec.tools[name]; dup {
			return nil, fmt.Errorf("%w: duplicate tool %q", ErrUnsupportedRequest, name)
		}
		compiled, err := compileDraft7(params)
		if err != nil {
			return nil, fmt.Errorf("%w: tool %q schema: %v", ErrUnsupportedRequest, name, err)
		}
		spec.tools[name] = compiled
		names = append(names, name)
		fmt.Fprintf(&catalog, "- %s: %s\n  arguments schema: %s\n", name, tool.Function.Description, params)
	}
	spec.catalog = catalog.String()

	// The CLI hands this schema to the API as a tool input schema, which needs a
	// root type: a root that was only a oneOf was rejected. The schema stays flat,
	// and which fields go with which kind, and each call's arguments, are checked
	// in parse.
	namesJSON, _ := json.Marshal(names)
	spec.schema = json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,"required":["kind"],`+
		`"properties":{"kind":{"type":"string","enum":[%q,%q]},"content":{"type":"string"},`+
		`"calls":{"type":"array","maxItems":%d,"items":{"type":"object","additionalProperties":false,`+
		`"required":["name","arguments"],"properties":{"name":{"type":"string","enum":%s},"arguments":{"type":"object"}}}}}}`,
		kindText, kindToolCalls, maxCallsPerDecision, namesJSON))

	whole, err := compileDraft7(spec.schema)
	if err != nil {
		return nil, fmt.Errorf("%w: decision schema: %v", ErrUnsupportedRequest, err)
	}
	spec.whole = whole

	return spec, nil
}

type decision struct {
	Kind    string          `json:"kind"`
	Content *string         `json:"content"`
	Calls   *[]decisionCall `json:"calls"`
}

type decisionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func validationError(result *gojsonschema.Result) error {
	msgs := make([]string, 0, maxReportedErrors)
	for i, e := range result.Errors() {
		if i == maxReportedErrors {
			break
		}
		msgs = append(msgs, e.String())
	}
	return fmt.Errorf("%w: %s", ErrInvalidDecision, strings.Join(msgs, "; "))
}

// parse turns the CLI's structured output into a reply. Every tool call it returns
// names a tool from the request and carries arguments valid against that tool's
// own schema, with an ID minted from idTemplate; anything else is an error.
func (s *decisionSpec) parse(raw json.RawMessage, idTemplate string) (string, []llms.ToolCall, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil, fmt.Errorf("%w: no structured output", ErrInvalidDecision)
	}

	result, err := s.whole.Validate(gojsonschema.NewBytesLoader(raw))
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalidDecision, err)
	}
	if !result.Valid() {
		return "", nil, validationError(result)
	}

	var d decision
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalidDecision, err)
	}
	switch d.Kind {
	case kindText:
		if d.Calls != nil || d.Content == nil || strings.TrimSpace(*d.Content) == "" {
			return "", nil, fmt.Errorf("%w: a text reply needs non-empty content and no calls", ErrInvalidDecision)
		}
		return *d.Content, nil, nil
	case kindToolCalls:
		if d.Calls == nil || len(*d.Calls) == 0 || (d.Content != nil && *d.Content != "") {
			return "", nil, fmt.Errorf("%w: a tool_calls reply needs at least one call and no content", ErrInvalidDecision)
		}
	default:
		return "", nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidDecision, d.Kind)
	}

	seen := make(map[string]struct{}, len(*d.Calls))
	calls := make([]llms.ToolCall, 0, len(*d.Calls))
	for i, call := range *d.Calls {
		toolSchema, ok := s.tools[call.Name]
		if !ok {
			return "", nil, fmt.Errorf("%w: calls[%d] names unknown tool %q", ErrInvalidDecision, i, call.Name)
		}
		argsResult, err := toolSchema.Validate(gojsonschema.NewBytesLoader(call.Arguments))
		if err != nil {
			return "", nil, fmt.Errorf("%w: calls[%d] arguments: %v", ErrInvalidDecision, i, err)
		}
		if !argsResult.Valid() {
			return "", nil, fmt.Errorf("calls[%d] %q: %w", i, call.Name, validationError(argsResult))
		}

		var compact bytes.Buffer
		if err := json.Compact(&compact, call.Arguments); err != nil {
			return "", nil, fmt.Errorf("%w: calls[%d] arguments: %v", ErrInvalidDecision, i, err)
		}

		id := templates.GenerateFromPattern(idTemplate, call.Name)
		for _, dup := seen[id]; dup; _, dup = seen[id] {
			id = templates.GenerateFromPattern(idTemplate, call.Name)
		}
		seen[id] = struct{}{}

		calls = append(calls, llms.ToolCall{
			ID:   id,
			Type: "function",
			FunctionCall: &llms.FunctionCall{
				Name:      call.Name,
				Arguments: compact.String(),
			},
		})
	}

	return "", calls, nil
}

func parseJSONObject(raw json.RawMessage) (string, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return "", fmt.Errorf("%w: structured output is not a JSON object", ErrInvalidDecision)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidDecision, err)
	}
	return compact.String(), nil
}
