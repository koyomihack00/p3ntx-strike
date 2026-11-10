package scopegate

import (
	"slices"
	"testing"
)

func TestShell_LexShell_Segments(t *testing.T) {
	cases := []struct {
		cmd  string
		want [][]string
	}{
		{"ls -la", [][]string{{"ls", "-la"}}},
		{"a | b && c; d & e || f", [][]string{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}, {"f"}}},
		{"grep 'a b' \"c d\"", [][]string{{"grep", "a b", "c d"}}},
		{"ls 2>&1 >out.txt", [][]string{{"ls", "out.txt"}}},
		{"ls &> all.log", [][]string{{"ls", "all.log"}}},
		{"(ls)\nid", [][]string{{"ls"}, {"id"}}},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			lx := lexShell(tc.cmd)
			var got [][]string
			for _, seg := range lx.segments {
				var words []string
				for _, w := range seg.words {
					words = append(words, w.text)
				}
				got = append(got, words)
			}
			if !slices.EqualFunc(got, tc.want, slices.Equal[[]string]) {
				t.Errorf("segments = %q, want %q", got, tc.want)
			}
			if len(lx.opaque) != 0 {
				t.Errorf("unexpected opaque reasons %q", lx.opaque)
			}
		})
	}
}

func TestShell_LexShell_ReportsOpaqueConstructs(t *testing.T) {
	for _, cmd := range []string{
		"echo $HOME", "echo ${x}", "echo $(id)", "echo `id`", "echo \"$x\"", "echo \"`id`\"",
		`echo a\ b`, "cat <<EOF", "diff <(ls) <(ls)", "tee >(cat)", "echo 'open", `echo "open`,
		"# hidden", "ls\x01", "ls >",
	} {
		if lx := lexShell(cmd); len(lx.opaque) == 0 {
			t.Errorf("%q lexed as fully understood", cmd)
		}
	}
}

func TestShell_CommandName(t *testing.T) {
	cases := []struct {
		cmd, name string
	}{
		{"ls -la", "ls"},
		{"/usr/bin/id", "id"},
		{"> out.txt ls", "ls"},
		{"'ls'", ""},
		{"/tmp/ls", ""},
		{"/usr/bin/../../tmp/ls", ""},
		{"VAR=1 ls", ""},
		{"if", ""},
		{"ｌs", ""},
	}
	for _, tc := range cases {
		lx := lexShell(tc.cmd)
		if len(lx.segments) == 0 {
			t.Fatalf("%q: no segment", tc.cmd)
		}
		name, reason := commandName(lx.segments[0])
		if name != tc.name || (name == "") == (reason == "") {
			t.Errorf("%q -> name %q reason %q, want name %q", tc.cmd, name, reason, tc.name)
		}
	}
}
