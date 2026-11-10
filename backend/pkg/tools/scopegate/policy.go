// Package scopegate decides, outside the model, whether a proposed tool call may
// run: the agent's tool allowlist, the engagement scope of every network target its
// arguments name, and which calls must wait for the owner's approval.
//
// It never resolves DNS: a hostname is in scope only when the policy lists it, so a
// name that happens to resolve into a scoped CIDR is still treated as out of scope.
package scopegate

import (
	"fmt"
	"net/netip"
	"strings"
)

// Verdict is ordered: combining verdicts keeps the lowest, and the zero value denies.
type Verdict int

const (
	Deny Verdict = iota
	RequireApproval
	Allow
)

func (v Verdict) String() string {
	switch v {
	case Deny:
		return "deny"
	case RequireApproval:
		return "require_approval"
	case Allow:
		return "allow"
	default:
		return fmt.Sprintf("verdict(%d)", int(v))
	}
}

type Scope struct {
	CIDRs []string
	// Hosts holds exact names, or "*.name" for strict subdomains of name.
	Hosts []string
}

type Policy struct {
	Scope Scope
	// AgentTools lists the tools each agent type may call; an agent absent from the
	// map may call nothing.
	AgentTools map[string][]string
	// AutoApproveBinaries are the commands that may run without approval, each one
	// of SafeLocalBinaries. Every other terminal command needs the owner's approval.
	AutoApproveBinaries []string
	// ExternalSearch applies to tools that send a query to a third-party search
	// service; its zero value denies them.
	ExternalSearch Verdict
}

// SafeLocalBinaries is the closed set a policy may auto-approve: commands that
// only read or format local data and can neither open a network connection nor
// start another program. sort (--compress-program), find (-exec), awk, sed, env,
// xargs and every interpreter are left out for that reason.
var SafeLocalBinaries = []string{
	"base64", "basename", "cat", "cmp", "cut", "date", "df", "diff", "dirname", "du",
	"echo", "egrep", "false", "fgrep", "file", "free", "grep", "head", "hexdump", "hostname",
	"id", "jq", "ls", "md5sum", "nproc", "od", "printf", "ps", "pwd", "realpath",
	"sha1sum", "sha256sum", "sleep", "stat", "strings", "tail", "tr", "true", "uname",
	"uniq", "wc", "which", "whoami", "xxd",
}

type compiledPolicy struct {
	prefixes       []netip.Prefix
	hosts          map[string]struct{}
	hostSuffixes   []string
	agentTools     map[string]map[string]struct{}
	autoApprove    map[string]struct{}
	externalSearch Verdict
}

func compile(p Policy) (*compiledPolicy, error) {
	if p.ExternalSearch < Deny || p.ExternalSearch > Allow {
		return nil, fmt.Errorf("invalid external search verdict %d", int(p.ExternalSearch))
	}

	cp := &compiledPolicy{
		hosts:          make(map[string]struct{}),
		agentTools:     make(map[string]map[string]struct{}, len(p.AgentTools)),
		autoApprove:    make(map[string]struct{}, len(p.AutoApproveBinaries)),
		externalSearch: p.ExternalSearch,
	}

	for _, raw := range p.Scope.CIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			addr, addrErr := netip.ParseAddr(strings.TrimSpace(raw))
			if addrErr != nil {
				return nil, fmt.Errorf("invalid scope CIDR %q: %w", raw, err)
			}
			prefix = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())
		}
		cp.prefixes = append(cp.prefixes, netip.PrefixFrom(prefix.Addr().Unmap(), unmappedBits(prefix)).Masked())
	}

	for _, raw := range p.Scope.Hosts {
		host := normalizeHost(raw)
		if suffix, ok := strings.CutPrefix(host, "*."); ok {
			if !isHostname(suffix) {
				return nil, fmt.Errorf("invalid scope host pattern %q", raw)
			}
			cp.hostSuffixes = append(cp.hostSuffixes, "."+suffix)
			continue
		}
		if !isHostname(host) {
			return nil, fmt.Errorf("invalid scope host %q", raw)
		}
		cp.hosts[host] = struct{}{}
	}

	for agent, tools := range p.AgentTools {
		set := make(map[string]struct{}, len(tools))
		for _, tool := range tools {
			set[tool] = struct{}{}
		}
		cp.agentTools[agent] = set
	}

	safe := make(map[string]bool, len(SafeLocalBinaries))
	for _, bin := range SafeLocalBinaries {
		safe[bin] = true
	}
	for _, bin := range p.AutoApproveBinaries {
		if !safe[bin] {
			return nil, fmt.Errorf("%q cannot be auto-approved: only SafeLocalBinaries can", bin)
		}
		cp.autoApprove[bin] = struct{}{}
	}

	return cp, nil
}

// unmappedBits keeps a ::ffff:a.b.c.d/n prefix meaningful after unmapping to IPv4.
func unmappedBits(prefix netip.Prefix) int {
	if prefix.Addr().Is4In6() {
		return max(prefix.Bits()-96, 0)
	}
	return prefix.Bits()
}

func (cp *compiledPolicy) toolAllowed(agent, tool string) bool {
	tools, ok := cp.agentTools[agent]
	if !ok {
		return false
	}
	_, ok = tools[tool]
	return ok
}

func (cp *compiledPolicy) addrInScope(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range cp.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (cp *compiledPolicy) prefixInScope(target netip.Prefix) bool {
	target = netip.PrefixFrom(target.Addr().Unmap(), unmappedBits(target)).Masked()
	for _, prefix := range cp.prefixes {
		if prefix.Bits() <= target.Bits() && prefix.Contains(target.Addr()) {
			return true
		}
	}
	return false
}

func (cp *compiledPolicy) hostInScope(host string) bool {
	if _, ok := cp.hosts[host]; ok {
		return true
	}
	for _, suffix := range cp.hostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}
