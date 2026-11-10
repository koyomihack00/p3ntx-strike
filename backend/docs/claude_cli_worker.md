# Claude CLI worker

`cmd/claudeworker` (Linux only) serves the `claudecli` adapter: one stateless Claude Code process per request, with no tools and no session, behind an authenticated HTTP endpoint. It is not part of the PentAGI image, and `cmd/pentagi` does not import it. The review-only deployment files are in `examples/claude-worker`.

## Startup

The binary exits with an error, naming the variable but never its value, if any of these holds:
- it runs as root;
- `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX` or `CLAUDE_CODE_USE_FOUNDRY` is in its environment;
- the CLI path is not an absolute executable;
- the work root is not a directory private to its user;
- a secret file is readable by group or others;
- the listen address is unspecified (`0.0.0.0`, `::`);
- the proxy is not `http://` on a loopback IP.

| Variable | Meaning |
|---|---|
| `CLAUDE_WORKER_LISTEN` | `unix:/path` or `tcp:IP:PORT` |
| `CLAUDE_WORKER_TOKEN_FILE` | Bearer token PentAGI must present (at least 32 characters) |
| `CLAUDE_WORKER_CLI` | Absolute path of the pinned CLI |
| `CLAUDE_WORKER_CLI_VERSION` | Reported in each result; the CLI is never run outside a request |
| `CLAUDE_WORKER_WORK_ROOT` | Parent of the per-call directories; a tmpfs is intended |
| `CLAUDE_WORKER_OAUTH_TOKEN_FILE` | Optional; read on every call and passed as `CLAUDE_CODE_OAUTH_TOKEN` |
| `CLAUDE_WORKER_HTTPS_PROXY` | Optional; passed as `HTTPS_PROXY` |
| `CLAUDE_WORKER_MAX_CONCURRENT` | Concurrent CLI processes (default 2); an extra request gets `overloaded` at once |
| `CLAUDE_WORKER_MAX_TIMEOUT` | Cap on the per-request timeout (default 10m) |
| `CLAUDE_WORKER_USE_OWNER_LOGIN` | `1` runs the CLI with the worker user's real `HOME`, so it uses that user's existing Claude login; excludes the OAuth token file. The worker never reads the login |
| `CLAUDE_WORKER_CAPTURE_DIR` | Optional private directory that receives each call's raw stream-json (0600), for diagnostics |

## HTTP API

- `GET /healthz` returns `ok`.
- `POST /v1/run` takes a `protocol.Request`.
  - It needs `Authorization: Bearer <token>`, compared in constant time.
  - The body is limited to 16 MiB, and unknown fields are rejected.
  - Success is 200 with a `protocol.Result`.
  - Failure is a status plus `{"kind","message"}`:

| Kind | Status |
|---|---|
| `bad_request` | 400 |
| `unauthorized` | 401 |
| `rate_limited` | 429 |
| `overloaded`, `unavailable` | 503 |
| `timeout` | 504 |
| `auth` | 502 |
| `internal` | 500 |

When the client disconnects, the request context ends and the call is cancelled.

## One call

1. **Validate the request.** The protocol version must match; the model name must match `^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,127}$`, so no flag can be smuggled in; the transcript must be non-empty; the timeout must be positive; the schema, if any, must be a JSON object under 100 KiB.
2. **Create `call-*/{home,config,cwd,tmp}`** (0700) under the work root and write the system prompt to a 0600 file. The whole directory is removed when the call ends.
3. **Build the environment from nothing:** `PATH`, `LANG`, `HOME`, `CLAUDE_CONFIG_DIR`, `TMPDIR`, the isolation switches (telemetry, error reporting, updates, non-essential traffic, CLAUDE.md, auto memory, background tasks, web fetch, bundled skills, prompt history), and optionally the OAuth token and `HTTPS_PROXY`. Nothing from the worker's own environment is inherited.
4. **Run the CLI** with fixed flags: `-p --output-format stream-json --verbose --restricted --tools "" --strict-mcp-config --mcp-config {"mcpServers":{}} --disallowedTools mcp__* --disable-slash-commands --no-session-persistence --model M --system-prompt-file F [--json-schema S] [--max-turns N]`. `--restricted` makes the CLI ignore user, project and local settings files, which matters in owner-login mode. `--bare` is not used: it never reads OAuth credentials. The transcript goes on stdin, the working directory is `cwd`, and the process leads its own process group.
5. **Read stream-json up to the `result` message.** On the way, record the tools of the `system/init` event, every MCP server it lists (`mcp_server:<name>`), and every content block whose type ends in `tool_use`. Output that is not valid stream-json, output over the limit (8 MiB by default), or an end of output without a result all fail closed.
6. **End the process group.**

| Situation | What happens |
|---|---|
| Cancellation or timeout | SIGTERM to the group, then SIGKILL after the grace period (3 s by default) |
| CLI exits | Output gets the grace period, then the group is killed (a lingering child dies) |
| Every path | One final SIGKILL to the group |

If output is still open after the kill, for example because a process left the group with `setsid` and holds stdout, the reader is closed so the call still ends. That process itself survives until the systemd unit's control-group kill. This is a known limit of process-group kill.

## What is never logged or returned

The worker logs one line per request containing request ID, agent, model, byte sizes, outcome, duration and evidence counts. It never logs or returns:
- the transcript or the system prompt;
- the schema;
- CLI stdout or stderr (stderr is counted, never shown);
- the bearer token, the OAuth token, or its file path.

Unclassified errors reach the client as `"worker failure"`.

## Observed with Claude Code 2.1.322 (Phase A, 2026-11-10)

- **Init event.** It carries `tools` and `mcp_servers`. The tools are `[]` in text mode and `["StructuredOutput"]` with `--json-schema`; MCP servers are `[]`; `apiKeySource` is `none` with a subscription login.
- **Also listed in init.** The CLI loads three built-in plugins (`cc-plugin-agents-md`, `cc-plugin-privacy`, `cc-plugin-plugin-authoring`) and lists built-in agents. Neither can run without the Task or Skill tools, which are absent.
- **Event types.** Besides `system/init`, `assistant`, `user` and `result`, the stream carries `rate_limit_event`, `system/informational` and `system/model_refusal_fallback`.
- **API errors.** An API error comes back as `subtype: success` with `is_error: true`, and the message in `result`.
- **Turns.** A structured-output reply takes 3 turns, a plain reply 2.

## Still unconfirmed

- That `--json-schema` accepts a schema of this size inline in argv.
- How the CLI reports a rate limit or an auth failure when run as `-p` with stream-json.
- That the CLI honours `HTTPS_PROXY` for every request.
