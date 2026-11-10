package scopegate

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type Target struct {
	Value string
	// Definite is false when the gate inferred the target: a name lifted out of
	// free shell text, or an address written in a non-canonical form.
	Definite bool
	addr     netip.Addr
	prefix   netip.Prefix
	rangeEnd netip.Addr
	host     string
}

var (
	hostnamePattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	ipv4RangePattern = regexp.MustCompile(`^(\d{1,3}\.\d{1,3}\.\d{1,3}\.)(\d{1,3})-(\d{1,3})$`)
)

// Shell tokens with these final labels are files far more often than hosts.
var fileExtensions = map[string]bool{
	"txt": true, "log": true, "xml": true, "json": true, "csv": true, "html": true, "htm": true,
	"yml": true, "yaml": true, "conf": true, "cfg": true, "ini": true, "sh": true, "py": true,
	"rb": true, "pl": true, "php": true, "js": true, "go": true, "c": true, "md": true, "out": true,
	"gz": true, "tgz": true, "zip": true, "tar": true, "bz2": true, "xz": true, "pcap": true,
	"pem": true, "key": true, "crt": true, "lst": true, "bak": true, "db": true, "sql": true,
	"nmap": true, "gnmap": true, "jar": true, "exe": true, "dll": true, "so": true, "bin": true,
}

func isHostname(host string) bool {
	if len(host) > 253 || !hostnamePattern.MatchString(host) {
		return false
	}
	labels := strings.Split(host, ".")
	tld := labels[len(labels)-1]
	return strings.IndexFunc(tld, func(r rune) bool { return r >= 'a' && r <= 'z' }) >= 0
}

func addrTarget(value string, addr netip.Addr, definite bool) Target {
	return Target{Value: value, Definite: definite, addr: addr.Unmap()}
}

// urlTarget accepts only http(s) URLs whose host is a canonical address or a valid
// hostname: the browser tool fetches nothing else, and anything looser is refused.
func urlTarget(raw string) (Target, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Target{}, fmt.Errorf("unparseable URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Target{}, fmt.Errorf("URL scheme %q is not http or https", u.Scheme)
	}
	host := normalizeHost(strings.Trim(u.Hostname(), "[]"))
	if addr, err := netip.ParseAddr(host); err == nil {
		return addrTarget(raw, addr, true), nil
	}
	if !isHostname(host) {
		return Target{}, fmt.Errorf("URL host %q is not a canonical address or hostname", u.Hostname())
	}
	return Target{Value: raw, Definite: true, host: host}, nil
}

// parseLooseIPv4 decodes the forms inet_aton accepts and canonical parsing does
// not: one to three parts, and octal or hex parts (134744072, 0x08080808, 10.1,
// 010.0.0.1). A bare decimal of at most 65535 is not taken for an address: it is
// a port or a count far more often.
func parseLooseIPv4(s string) (netip.Addr, bool) {
	if s == "" || strings.Trim(s, "0123456789abcdefABCDEFxX.") != "" || !strings.ContainsAny(s, "0123456789") {
		return netip.Addr{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		v, ok := parseInetPart(p)
		if !ok {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	if len(parts) == 1 && isDigits(s) && !strings.HasPrefix(s, "0") && vals[0] <= 65535 {
		return netip.Addr{}, false
	}

	var ip uint64
	switch len(parts) {
	case 1:
		ip = vals[0]
	case 2:
		if vals[0] > 0xff || vals[1] > 0xffffff {
			return netip.Addr{}, false
		}
		ip = vals[0]<<24 | vals[1]
	case 3:
		if vals[0] > 0xff || vals[1] > 0xff || vals[2] > 0xffff {
			return netip.Addr{}, false
		}
		ip = vals[0]<<24 | vals[1]<<16 | vals[2]
	case 4:
		for _, v := range vals {
			if v > 0xff {
				return netip.Addr{}, false
			}
		}
		ip = vals[0]<<24 | vals[1]<<16 | vals[2]<<8 | vals[3]
	}
	if ip > 0xffffffff {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{byte(ip >> 24), byte(ip >> 16), byte(ip >> 8), byte(ip)}), true
}

func parseInetPart(p string) (uint64, bool) {
	var (
		v   uint64
		err error
	)
	switch {
	case p == "":
		return 0, false
	case strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X"):
		if len(p) == 2 {
			return 0, false
		}
		v, err = strconv.ParseUint(p[2:], 16, 32)
	case len(p) > 1 && p[0] == '0':
		v, err = strconv.ParseUint(p[1:], 8, 32)
	default:
		v, err = strconv.ParseUint(p, 10, 32)
	}
	return v, err == nil
}

// hostTarget resolves a host string found in shell text. A canonical address is
// definite; a loose IPv4 form or a hostname is not. ok is false when the string
// is none of these.
func hostTarget(value, host string, definite bool) (Target, bool) {
	h := normalizeHost(strings.Trim(host, "[]"))
	if addr, err := netip.ParseAddr(h); err == nil {
		return addrTarget(value, addr, true), true
	}
	if addr, ok := parseLooseIPv4(h); ok {
		return addrTarget(value, addr, false), true
	}
	if isHostname(h) {
		return Target{Value: value, Definite: definite, host: h}, true
	}
	return Target{}, false
}

func isSchemeChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '+' || b == '.' || b == '-'
}

var knownSchemes = []string{"https", "http", "ftps", "ftp", "wss", "ws", "smb", "ldaps", "ldap", "gopher", "dict", "file", "tftp", "telnet", "ssh", "rtsp", "imaps", "imap", "pop3s", "pop3", "smtps", "smtp", "mqtt", "redis", "mongodb", "postgres", "mysql"}

// schemeStart finds where the URL scheme in text[from:idx] begins: a known scheme
// as a suffix of that run (so "-uhttp" yields "http"), or else the first letter.
func schemeStart(text string, from, idx int) int {
	run := strings.ToLower(text[from:idx])
	for _, scheme := range knownSchemes {
		if strings.HasSuffix(run, scheme) {
			return idx - len(scheme)
		}
	}
	for from < idx && !(text[from] >= 'a' && text[from] <= 'z' || text[from] >= 'A' && text[from] <= 'Z') {
		from++
	}
	return from
}

func hasNonASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) >= 0
}

// wordTargets extracts the network targets a shell word names. unparsed reports a
// word that looks like a network target but could not be read; the gate must not
// treat such a word as harmless.
func wordTargets(word string) (targets []Target, unparsed []string) {
	for _, piece := range strings.Split(word, ",") {
		t, bad := pieceTargets(piece)
		targets = append(targets, t...)
		if bad {
			unparsed = append(unparsed, piece)
		}
	}
	return targets, unparsed
}

func pieceTargets(text string) ([]Target, bool) {
	if strings.HasPrefix(text, "-") {
		if _, value, ok := strings.Cut(text, "="); ok {
			text = value
		} else if !strings.Contains(text, "://") {
			return nil, false
		}
	}
	if text == "" {
		return nil, false
	}

	if idx := strings.Index(text, "://"); idx >= 0 {
		start := idx
		for start > 0 && isSchemeChar(text[start-1]) {
			start--
		}
		start = schemeStart(text, start, idx)
		raw := text[start:]
		u, err := url.Parse(raw)
		if start == idx || err != nil || u.Host == "" {
			return nil, true
		}
		t, ok := hostTarget(raw, u.Hostname(), true)
		if !ok {
			return nil, true
		}
		return []Target{t}, false
	}

	if i := strings.LastIndex(text, "@"); i >= 0 {
		text = text[i+1:]
	}

	if m := ipv4RangePattern.FindStringSubmatch(text); m != nil {
		start, errStart := netip.ParseAddr(m[1] + m[2])
		end, errEnd := netip.ParseAddr(m[1] + m[3])
		if errStart != nil || errEnd != nil {
			return nil, true
		}
		return []Target{{Value: text, Definite: true, addr: start, rangeEnd: end}}, false
	}
	if prefix, err := netip.ParsePrefix(text); err == nil {
		return []Target{{Value: text, Definite: true, prefix: prefix}}, false
	}
	if addr, err := netip.ParseAddr(text); err == nil {
		return []Target{addrTarget(text, addr, true)}, false
	}
	if host, _, err := net.SplitHostPort(text); err == nil {
		if t, ok := hostTarget(text, host, false); ok {
			return []Target{t}, false
		}
		if strings.ContainsAny(host, ".:") || hasNonASCII(host) {
			return nil, true
		}
		return nil, false
	}
	if addr, ok := parseLooseIPv4(text); ok {
		return []Target{addrTarget(text, addr, false)}, false
	}

	host, _, _ := strings.Cut(text, "/")
	host = normalizeHost(host)
	if isHostname(host) {
		labels := strings.Split(host, ".")
		if fileExtensions[labels[len(labels)-1]] {
			return nil, false
		}
		return []Target{{Value: text, host: host}}, false
	}
	if hasNonASCII(host) && strings.ContainsAny(host, ".。．｡") {
		return nil, true
	}
	return nil, false
}

// localOnly reports addresses that name the sandbox itself or a bind-all socket
// rather than a remote target.
func (t Target) localOnly() bool {
	if !t.addr.IsValid() || t.rangeEnd.IsValid() {
		return false
	}
	return t.addr.IsLoopback() || t.addr.IsUnspecified()
}

func (cp *compiledPolicy) targetInScope(t Target) bool {
	switch {
	case t.prefix.IsValid():
		return cp.prefixInScope(t.prefix)
	case t.rangeEnd.IsValid():
		for _, prefix := range cp.prefixes {
			if prefix.Contains(t.addr) && prefix.Contains(t.rangeEnd) && !t.rangeEnd.Less(t.addr) {
				return true
			}
		}
		return false
	case t.addr.IsValid():
		return cp.addrInScope(t.addr)
	default:
		return cp.hostInScope(t.host)
	}
}

type commandFacts struct {
	targets  []Target
	unparsed []string
	commands []string
	opaque   []string
}

func analyzeCommand(cmd string) commandFacts {
	lx := lexShell(cmd)
	f := commandFacts{opaque: lx.opaque}
	addOpaque := func(reason string) {
		for _, r := range f.opaque {
			if r == reason {
				return
			}
		}
		f.opaque = append(f.opaque, reason)
	}

	for _, seg := range lx.segments {
		name, reason := commandName(seg)
		if reason != "" {
			addOpaque(reason)
		}
		f.commands = append(f.commands, name)
		for _, w := range seg.words {
			if strings.Contains(w.text, "/dev/tcp/") || strings.Contains(w.text, "/dev/udp/") {
				addOpaque("network pseudo-device")
			}
			ts, bad := wordTargets(w.text)
			f.targets = append(f.targets, ts...)
			f.unparsed = append(f.unparsed, bad...)
		}
	}
	return f
}
