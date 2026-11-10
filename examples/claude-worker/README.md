# Claude CLI worker: review-only deployment files

**Nothing in this directory is installed, enabled or applied by any build, script or service.** These files are for review at Gate G2. Applying them is a later gate, and it needs explicit approval.

The worker (`backend/cmd/claudeworker`) runs Claude Code headless calls for the `claudecli` provider adapter. It must run as a dedicated unprivileged user, never inside the `pentagi` container, which holds the Docker socket and runs as root.

## Files

| File | Purpose |
|---|---|
| `create-claude-worker-user.sh` | Creates the `claude-worker` system user and its directories. It refuses to do anything without `--apply`, and refuses if the user would be in the `docker` group |
| `claude-worker.service` | systemd unit with sandboxing, a cgroup-level IP allowlist (loopback only), control-group kill, and resource limits |
| `worker.env.example` | Non-secret settings read through `EnvironmentFile=` |
| `claude-worker.nft` | nftables rules keyed on the socket owner: the worker user may reach only the local proxy; the proxy user may reach only TCP 443 outside lab ranges |
| `egress-proxy.md` | What the allowlisting HTTPS proxy must allow, and what is still unconfirmed |

## Layout

| Path | Owner / mode | Content |
|---|---|---|
| `/opt/claude-worker/bin/claudeworker` | root:root 0755 | Worker binary (`go build ./cmd/claudeworker`) |
| `/opt/claude-code/bin/claude` | root:root 0755 | The pinned Claude Code CLI; never auto-updated |
| `/etc/claude-worker/worker.env` | root:claude-worker 0640 | Non-secret settings |
| `/etc/claude-worker/bearer-token` | claude-worker 0600 | Token PentAGI presents to the worker; at least 32 characters |
| `/etc/claude-worker/oauth-token` | claude-worker 0600 | Output of `claude setup-token`, created by the owner at Gate G3 and never by an agent |
| `/run/claude-worker/` | tmpfs, claude-worker 0700 | `worker.sock` (0660) and the per-call work root `calls/` (0700) |

## Defence layers, in order

1. **The worker refuses to start** if it is root, or if `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or a cloud-provider switch is in its environment.
2. **Each call gets a clean process.** Its environment is built from nothing, it has a fresh `HOME`, `CLAUDE_CONFIG_DIR`, `TMPDIR` and working directory, the transcript arrives on stdin, and the flags are fixed in code (`--tools ""`, strict empty MCP config, `--disallowedTools "mcp__*"`, no slash commands, no session persistence).
3. **The CLI runs in its own process group.** On cancellation, timeout or end of call the whole group receives SIGTERM, then SIGKILL. `KillMode=control-group` catches a process that escaped the group.
4. **Every call is verified.** The adapter rejects any call whose stream shows a tool other than `StructuredOutput`, an MCP server, or a permission denial.
5. **Network.** systemd `IPAddressAllow=localhost` plus the nftables owner match leave the worker able to reach only the local proxy, and the proxy allowlists Anthropic endpoints.

## Applying (later gate, not now)

1. Review and run `create-claude-worker-user.sh --apply` as root.
2. Install the pinned CLI and the worker binary under `/opt`.
3. Write the bearer token, then let the owner create the OAuth token with `claude setup-token`.
4. Install and configure the proxy per `egress-proxy.md`, then load `claude-worker.nft`.
5. Copy the unit, run `systemctl daemon-reload`, and start the worker.
6. Mount `/run/claude-worker/worker.sock` into the `pentagi` container.
