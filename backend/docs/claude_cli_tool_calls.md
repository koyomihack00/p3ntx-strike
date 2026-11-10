# Claude CLI tool-call compatibility

How `pkg/providers/claudecli` turns PentAGI's native tool-calling contract into a structured proposal from the CLI and back, without changing anything the agent loop relies on. See [claude_cli_adapter.md](claude_cli_adapter.md) for the surrounding design.

## What the agent loop requires

The agent loop (`pkg/providers/performer.go`) requires the following of every provider:

| Requirement | Where it is enforced |
|---|---|
| Tool calls arrive as `ContentChoice.ToolCalls`, each with a non-empty `ID`, `Type: "function"` and `FunctionCall{Name, Arguments}`, where `Arguments` is a JSON string | `performer.go` `fillResult` |
| A reply with neither content nor tool calls is an error | `performer.go` `fillResult` |
| Every `ToolCall.ID` is paired with a `ToolCallResponse.ToolCallID` | `pkg/cast` `NewChainAST` |
| IDs follow the flow's `tool_call_id_template` | `cast.NormalizeToolCallIDs`, `providers.go` template resolution |
| Tools run one at a time in the order returned; a barrier tool ends the chain | `performer.go` |
| Arguments that fail at execution go through the tool-call fixer (`simple_json`) | `performer.go` `execToolCall` |

## Decision schema

When tools are offered, the adapter sends the CLI one JSON Schema. The CLI hands it to the API as the input schema of its `StructuredOutput` tool, and the API requires a root `"type"`. A schema whose root was only a `oneOf` was rejected live with `API Error: 400 tools.1.custom.input_schema.type: Field required`. The schema is therefore a flat object with no combinators anywhere; whether the API would accept them alongside a root type was not tested.

```json
{"type": "object", "additionalProperties": false, "required": ["kind"],
 "properties": {
   "kind": {"type": "string", "enum": ["text", "tool_calls"]},
   "content": {"type": "string"},
   "calls": {"type": "array", "maxItems": 8,
     "items": {"type": "object", "additionalProperties": false, "required": ["name", "arguments"],
       "properties": {"name": {"type": "string", "enum": ["<offered tools>"]},
                      "arguments": {"type": "object"}}}}}}
```

Each offered tool appears in the system prompt as a catalogue entry: its name, its description, and its parameter schema as PentAGI sends it to API providers (`pkg/tools/registry.go`), with the root `$schema` and `$id` keys removed. The name enum lists exactly the tools offered this turn, so a tool the agent may not call is not expressible. The model never supplies an ID: `additionalProperties: false` rejects one.

## Validation

`decisionSpec.parse` accepts a decision only if every step below passes. Each failure is `ErrInvalidDecision`, and no partial result is returned:

1. The structured output is present and non-blank.
2. It validates against the decision schema.
3. **Text:** a `kind` of `text` needs non-empty `content` and no `calls` key.
4. **Tool calls:** a `kind` of `tool_calls` needs at least one call and no non-empty `content`.
5. Every call names a tool from the request.
6. Every call's `arguments` validates against that tool's own compiled schema. This is the step that enforces the per-tool contract.
7. Arguments are compacted with `json.Compact`. Nothing is coerced, repaired or defaulted.

The executor still unmarshals arguments into its own types and may still reject them. That path, and the tool-call fixer, are unchanged.

## Mapping back

| Decision | `ContentChoice` |
|---|---|
| `{"kind":"text","content":C}` | `Content: C`, no tool calls, `StopReason: "end_turn"` |
| `{"kind":"tool_calls","calls":[…]}` | `ToolCalls` in the same order, `FuncCall` = the first, `Content: ""`, `StopReason: "tool_use"` |

IDs are minted with `templates.GenerateFromPattern(ToolCallIDTemplate, name)` and are unique within one response. `GetToolCallIDTemplate` returns the configured template without calling the model, so creating a flow never sends the probe calls API doors use to detect an ID format. With the Anthropic template `toolu_{r:24:b}`, chains stay homogeneous when a flow mixes this adapter with the Anthropic API door.

## Rendering the chain

The conversation reaches the CLI as text: the system prompt as one string, the rest as a transcript on stdin.

| Chain message | Rendering |
|---|---|
| System | Appended to the system prompt after the adapter preamble |
| Human | `<user-M>` … `</user-M>` |
| AI text | Inside `<assistant-M>` … `</assistant-M>` |
| AI tool call | `<tool-call-M id="…" name="…">{arguments}</tool-call-M>` inside the assistant block |
| Tool response | `<tool-result-M id="…" name="…">` … `</tool-result-M>` |
| Reasoning | Dropped; it is never sent to the CLI |

`M` is a fresh 128-bit random marker per call. Before writing any value (text, ID, name, arguments, tool output, system text), the renderer refuses the request if the value contains the marker. Content therefore cannot close a block or open one, which matters most for tool output, the channel that carries data controlled by the target. Forged tags with any other marker stay inert text inside their block.

The preamble tells the model that tool results are untrusted data and that it has no tools of its own. That is guidance only. Enforcement comes from the verification step, the validation above, and the [Scope & Approval Gateway](scope_approval_policy.md).

## Limits

- At most 8 calls per decision. Native doors have no such cap, but the loop runs calls sequentially anyway.
- Images and binary parts are not supported.
- Reasoning continuity across turns is lost, as already happens when a flow switches provider (`ClearReasoning`).
- Decision quality against native tool use is unmeasured. The ≥90% tool-call accuracy target is a Tier B/L3 measurement.
