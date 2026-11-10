# Egress proxy for the Claude CLI worker (review-only)

The worker's only route out is an HTTPS forward proxy on `127.0.0.1:3128`, given to the CLI as `HTTPS_PROXY` through `CLAUDE_WORKER_HTTPS_PROXY`. The proxy, not an IP list, decides which hosts the CLI may reach. Anthropic's address ranges change, and the proxy sees the hostname in `CONNECT`.

## Allowlist (CONNECT to port 443 only)

| Host | Why | Status |
|---|---|---|
| `api.anthropic.com` | Model requests | Required |

With `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_TELEMETRY`, `DISABLE_ERROR_REPORTING` and the update switches set, no other host should be needed.

**Unconfirmed until Tier B:**
- whether a `setup-token` OAuth token ever triggers a request to another Anthropic host (for example a token check);
- whether the CLI honours `HTTPS_PROXY` for every request it makes.

Tier B must run with the proxy logging every `CONNECT`. Any host other than those above gets a decision in writing before it is added.

## Requirements on the proxy

- Run it as its own unprivileged user (`proxy` in `claude-worker.nft`), not as `claude-worker`.
- Allow `CONNECT` to the hosts above on 443 only, and deny plain HTTP and every other method.
- Listen on loopback only.
- Log host, port, bytes and outcome. Never log bodies; TLS is not intercepted.
- Installing a proxy package is a dependency change and needs its own approval.
