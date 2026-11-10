package scopegate

import (
	"slices"
	"testing"
)

func TestTargets_AnalyzeCommand(t *testing.T) {
	cases := []struct {
		cmd          string
		wantTargets  []string
		wantCommands []string
		wantUnparsed []string
		wantOpaque   bool
	}{
		{"nmap -sV 10.0.0.1", []string{"10.0.0.1"}, []string{"nmap"}, nil, false},
		{"cat out.txt | grep 80 && curl http://a.lab:8080/x", []string{"http://a.lab:8080/x"}, []string{"cat", "grep", "curl"}, nil, false},
		{"ssh admin@10.0.0.2 'id'", []string{"10.0.0.2"}, []string{"ssh"}, nil, false},
		{"curl http://intranet/ http://ｅvil.com/", nil, []string{"curl"}, []string{"http://intranet/", "http://ｅvil.com/"}, false},
		{"nmap 10.0.0.1,10.0.0.2", []string{"10.0.0.1", "10.0.0.2"}, []string{"nmap"}, nil, false},
		{"curl -uhttp://x.lab/a", []string{"http://x.lab/a"}, []string{"curl"}, nil, false},
		{"ls `pwd`", nil, []string{"ls"}, nil, true},
		{"echo hi > /tmp/report.md", nil, []string{"echo"}, nil, false},
		{"FOO=1 nmap host.lab", []string{"host.lab"}, []string{""}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			facts := analyzeCommand(tc.cmd)
			var targets []string
			for _, target := range facts.targets {
				targets = append(targets, target.Value)
			}
			if !slices.Equal(targets, tc.wantTargets) {
				t.Errorf("targets = %q, want %q", targets, tc.wantTargets)
			}
			if !slices.Equal(facts.commands, tc.wantCommands) {
				t.Errorf("commands = %q, want %q", facts.commands, tc.wantCommands)
			}
			if !slices.Equal(facts.unparsed, tc.wantUnparsed) {
				t.Errorf("unparsed = %q, want %q", facts.unparsed, tc.wantUnparsed)
			}
			if (len(facts.opaque) > 0) != tc.wantOpaque {
				t.Errorf("opaque = %q, want %v", facts.opaque, tc.wantOpaque)
			}
		})
	}
}

func TestTargets_ParseLooseIPv4(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"134744072", "8.8.8.8"},
		{"0x08080808", "8.8.8.8"},
		{"0x8.0x8.0x8.0x8", "8.8.8.8"},
		{"010.010.010.005", "8.8.8.5"},
		{"10.1", "10.0.0.1"},
		{"8.8.2056", "8.8.8.8"},
		{"0177.1", "127.0.0.1"},
		{"65536", "0.1.0.0"},
		{"53", ""},
		{"8080", ""},
		{"1.2.3.4.5", ""},
		{"256.1.1.1", ""},
		{"0x", ""},
		{"dead.beef", ""},
		{"10.1.1.08", ""},
	}
	for _, tc := range cases {
		addr, ok := parseLooseIPv4(tc.in)
		got := ""
		if ok {
			got = addr.String()
		}
		if got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
}
