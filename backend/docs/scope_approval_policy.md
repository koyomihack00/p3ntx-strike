# Scope & Approval Gateway

Status: wired. `customExecutor.Execute` (`pkg/tools/executor.go`) asks the gate about every tool call after the handler lookup and before the handler runs, for every agent executor and every provider, API ones included. The controller installs the gate on each flow and assistant executor (`newGatedFlowToolsExecutor`), and an executor without one refuses every call. There is no approval mechanism yet, so `RequireApproval` is refused like `Deny`. Accepted, unremediated risks are in [claude_cli_risk_register.md](claude_cli_risk_register.md).

## Goal

Every tool call an agent proposes is judged **outside the model**, before its handler runs, against an engagement policy the owner sets. The model can propose anything. Only the gate decides. Anything the gate cannot classify with certainty needs the owner's approval: a parse failure never becomes an allow.

## Policy

| Field | Meaning | Zero value |
|---|---|---|
| `Scope.CIDRs` | IPv4/IPv6 prefixes or single addresses that are in scope | nothing in scope |
| `Scope.Hosts` | Exact hostnames, or `*.name` for strict subdomains of `name` (the apex is not included) | nothing in scope |
| `AgentTools` | Agent type → tools it may call | every agent may call nothing |
| `AutoApproveBinaries` | Terminal commands that run without approval; each must be in `SafeLocalBinaries` | every terminal command needs approval |
| `ExternalSearch` | Verdict for tools that send a query to a third-party search service | deny |

`SafeLocalBinaries` is a closed set defined in code. It holds local utilities that only read or format data and can neither open a network connection nor start another program (`cat`, `grep`, `ls`, `id`, `jq`, …). `sort`, `find`, `awk`, `sed`, `env`, `xargs` and every interpreter are excluded because each can start a program. `New` refuses a policy that auto-approves anything outside the set.

An invalid CIDR, host pattern, verdict or auto-approve entry makes `New` fail; the gate never silently drops a rule.

## Verdicts

The gate returns `Deny`, `RequireApproval` or `Allow`. Verdicts combine by taking the most restrictive, and the zero value is `Deny`. Every non-allow verdict carries reasons that name what was not understood, because the approver relies on them.

A decision also lists the targets found and a fingerprint: SHA-256 over flow, agent, tool and canonicalised arguments, where key order does not matter but every value does.

## Evaluation order

1. **Allowlist.** If the tool is not in `AgentTools[agent]`, the call is denied.
2. **Classification.** Every tool in `tools.GetRegistryDefinitions()` has a class, and a guard test fails when a new tool is added without one. An unclassified tool is denied.

| Class | Tools | Rule |
|---|---|---|
| no target | delegation, memory, result, barrier, flow-control tools, and `file` | allowed once on the allowlist |
| browser | `browser` | The `url` must be http(s), and its host a canonical IP or a valid hostname; anything else is denied. Loopback is not exempt |
| terminal | `terminal` | Below |
| external search | `google`, `duckduckgo`, `tavily`, `firecrawl`, `traversaal`, `perplexity`, `searxng`, `sploitus`, `web_search` | Follows `ExternalSearch` |

3. **Arguments.** Arguments are decoded into the executor's own types (`tools.TerminalAction`, `tools.Browser`), so the gate judges exactly what would run. Arguments that do not decode are denied.

## Terminal commands

The tool runs its input with `sh -c`, which is dash or bash depending on the sandbox image. The lexer (`shell.go`) understands only what both shells agree on.

**Allow.** A command runs without approval only when both hold:
- the lexer understood all of it;
- every segment, split on `; | & && || ( )` and newlines, runs a literal command name in `AutoApproveBinaries`, either bare or under `/bin`, `/usr/bin`, `/usr/local/bin`, `/sbin` or `/usr/sbin`.

Those commands cannot reach the network, so their arguments are not read as targets: `echo http://evil.example` is allowed.

**Opaque** (forces approval):
- any `$` (parameter, command or arithmetic expansion, including `$'…'`);
- a backtick;
- a backslash outside single quotes;
- `$`, a backtick or a backslash inside double quotes;
- process substitution, here-documents, comments;
- control characters, unterminated quotes, a redirection without a target;
- `/dev/tcp` or `/dev/udp` anywhere;
- a quoted or escaped command name;
- a `VAR=value` prefix;
- shell keywords (`if`, `!`, `{`, `[[`, `time`, …);
- command paths outside the standard directories;
- empty input.

**Every other command needs approval.** For those, the gate reads targets from every word, including redirect targets, comma lists, `--opt=value` and glued options such as `-uhttp://…`:

| Word | Target | Definite |
|---|---|---|
| `scheme://host…` | URL host | yes, when the host is a canonical IP or a hostname |
| canonical IPv4/IPv6, `user@addr`, `addr:port`, CIDR, `a.b.c.N-M` | address, prefix or range | yes |
| `inet_aton` forms: `134744072`, `0x08080808`, `10.1`, `010.0.0.1` | the decoded address | no |
| `name.tld`, `user@name`, `name:port`, `name/path` | hostname (not when the last label is a common file extension) | no |
| a URL whose host or syntax does not parse, a non-ASCII dotted name | **unparsed**: named in the reasons, never dropped | — |

A definite target out of scope is `Deny`. A non-definite target out of scope adds a reason; the command needs approval regardless.

Loopback and unspecified addresses are skipped in terminal commands, on the assumption that they name the sandbox itself. That assumption is wrong in `DOCKER_NETWORK=host` mode; see risk M-6 in the register.

DNS is never resolved. A hostname is in scope only if the policy names it.

## Approval

`Authorize(ctx, call, approver)` returns a nil error only when the call may run now:

- `Allow`: runs; the approver is not consulted.
- `Deny`: refused with `ErrDenied`; the approver is **never** consulted, and nothing lifts a deny.
- `RequireApproval`: the `Approver` receives the call and the full decision. Any of the following leaves the call refused with `ErrDenied`: no approver, an approver error, a rejection, a cancelled context, or an approval that arrives after the context ended.

Approvals are single-use.

## Known limits

- **Approval volume.** Any terminal command that is not a safe local utility needs approval, including in-scope scanners. That is the price of failing closed until per-tool argument parsers exist.
- **What an approved command reaches.** An approved command can reach anything its process can; network-level containment of the lab zone is an accepted, unremediated risk (H-4).
- **Targets in files.** Targets read from files (`nmap -iL`, `curl -K`) are invisible; such commands still need approval.

## Configuration

The policy is built from the server's environment at boot, and again for each flow or assistant executor; an entry that does not parse stops the server.

| Variable | Meaning | Default |
|---|---|---|
| `SCOPE_GATE_CIDRS` | `Scope.CIDRs`, comma-separated | empty: no address is in scope |
| `SCOPE_GATE_HOSTS` | `Scope.Hosts`, comma-separated | empty: no host is in scope |
| `SCOPE_GATE_EXTERNAL_SEARCH` | `allow` or `deny` | `deny` |

`AgentTools` gives every agent that owns an executor every classified tool: which tools an agent is offered is already fixed where its executor is built, so the gate does not keep a second copy. An agent outside that list, an executor built without an agent, and a tool with no class are refused. `AutoApproveBinaries` is the whole of `SafeLocalBinaries`.

## What the executor does with a refusal

The refusal text, prefixed with "this tool call was refused and was not run", becomes the tool's answer, so the model reads it instead of retrying. The tool call log records it as failed, the operator's message log shows it, and the server logs it at warning level with the agent. The handler never runs.

## What is still open

1. An `Approver` over the flow's waiting state and the UI; until it exists every network command is refused, even against an in-scope target.
2. A policy stored per flow and editable by the owner, instead of one per server.
3. Network-level containment of the sandbox and the scraper (risk H-4). The gate reads commands; it cannot see what an approved program does, so command inspection alone is not a scope boundary once approvals exist.
