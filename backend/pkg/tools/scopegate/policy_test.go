package scopegate

import "testing"

func TestPolicy_Compile_RejectsInvalidPolicy(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Policy)
	}{
		{"bad cidr", func(p *Policy) { p.Scope.CIDRs = []string{"10.10.10.0/33"} }},
		{"hostname as cidr", func(p *Policy) { p.Scope.CIDRs = []string{"target.lab"} }},
		{"bare wildcard", func(p *Policy) { p.Scope.Hosts = []string{"*"} }},
		{"wildcard in the middle", func(p *Policy) { p.Scope.Hosts = []string{"a.*.lab"} }},
		{"url as host", func(p *Policy) { p.Scope.Hosts = []string{"http://target.lab"} }},
		{"verdict out of range", func(p *Policy) { p.ExternalSearch = Verdict(7) }},
		{"network tool auto-approved", func(p *Policy) { p.AutoApproveBinaries = []string{"ls", "curl"} }},
		{"interpreter auto-approved", func(p *Policy) { p.AutoApproveBinaries = []string{"sh"} }},
		{"auto-approve by another case", func(p *Policy) { p.AutoApproveBinaries = []string{"LS"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := labPolicy()
			tc.mutate(&p)
			if _, err := New(p); err == nil {
				t.Error("New accepted an invalid policy")
			}
		})
	}
}

func TestPolicy_Compile_ZeroPolicyDeniesEverything(t *testing.T) {
	g := newGate(t, Policy{})
	if d := g.Evaluate(terminal("id")); d.Verdict != Deny {
		t.Errorf("verdict = %s, want deny", d.Verdict)
	}
}

func TestPolicy_Scope_IPv4MappedAddressesMatch(t *testing.T) {
	g := newGate(t, labPolicy())
	if d := g.Evaluate(browser("http://[::ffff:10.10.10.5]/")); d.Verdict != Allow {
		t.Errorf("verdict = %s, want allow", d.Verdict)
	}
}
