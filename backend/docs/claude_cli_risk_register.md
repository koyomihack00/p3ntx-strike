# Claude CLI integration: risk register

Owner decision of 2026-11-10 (Gate G2.6). P3NTX STRIKE is a local, single-user Kali VM. H-1 and H-2 are fixed. Every other finding of the G2.5 review is **accepted and not remediated**. An accepted risk stays open until a later change fixes it and a test proves the fix.

| ID | Status | Risk | Impact if exploited | Residual exposure on this VM |
|---|---|---|---|---|
| H-1 | **Fixed** (G2.6) | The gate dropped targets it could not parse, so commands were allowed | An out-of-scope or obfuscated target ran without approval | Unparseable targets now force approval and are named in the reasons. The remaining limit is targets read from files, which still need approval |
| H-2 | **Fixed** (G2.6) | Commands hidden behind keywords, quoting or unlisted wrappers escaped the approval list | A scanner ran without approval | Only commands in the code-defined `SafeLocalBinaries` set run without approval; everything else needs it |
| H-3 | **Accepted** | The `pentagi` container is root and holds `docker.sock` | It can mount and read any host file, including a Claude OAuth token and the owner's `~/.claude` login; so can sandbox commands when `DOCKER_INSIDE=true` | A compromise of PentAGI, or a `DOCKER_INSIDE=true` sandbox, exposes Claude credentials. Keep `DOCKER_INSIDE=false` |
| H-4 | **Accepted** | Sandbox and scraper egress are not restricted to the engagement scope | An approved or misjudged command, or a page the browser loads, can reach the internet, the home LAN, or host services on non-loopback addresses | While approvals are refused, only `SafeLocalBinaries` run in the sandbox, so its exposure is the gate's lexer. The scraper renders the page itself, so the gate judges only the URL it is handed, not where that page redirects or what it loads. An approval mechanism reopens this fully: network enforcement must come first |
| M-1 | **Accepted** | A CLI descendant that calls `setsid` escapes the process-group kill | The process survives with `CLAUDE_CODE_OAUTH_TOKEN` in its environment | The call still ends. The survivor runs until the worker unit stops; `TasksMax` bounds the count |
| M-2 | **Accepted** | Missing stream evidence counts as clean | A CLI version that reports tools differently could hide native tool use from `verify` | Mitigated only by `--tools ""` and the pinned CLI version |
| M-3 | **Accepted** | The unit's `IPAddressAllow=localhost` allows every loopback port | If the CLI gained execution, it could reach Postgres (default credentials), the PentAGI API and the scraper | Depends on the nft owner rules being loaded |
| M-4 | **Accepted** | The review-only nft file lacks `ct state established` | The proxy egress path does not work as written; it fails closed | The deployment files must be corrected before use |
| M-5 | **Accepted** | The worker HTTP server has no read or idle timeouts or connection cap, and decodes before the slot check | Local denial of service or worker OOM | Reachable only by local users (socket) or the bearer holder |
| M-6 | **Accepted** | The gate skips loopback in terminal commands | In `DOCKER_NETWORK=host` mode, `curl 127.0.0.1:5432` targets the host | Such commands need approval, which is refused today, unless the binary is a safe local utility. Do not use host network mode |
| L-1…L-8, I-1…I-5 | **Accepted** | See the G2.5 review | Low | Unchanged |
