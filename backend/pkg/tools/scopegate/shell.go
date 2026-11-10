package scopegate

import (
	"path"
	"regexp"
	"strings"
)

// The terminal tool runs its input as `sh -c`, which is dash or bash depending on
// the sandbox image. The lexer understands only the subset both shells agree on and
// reports everything else as opaque, so the gate never allows a command whose
// meaning it had to guess.

type shellWord struct {
	text           string
	quoted         bool
	redirectTarget bool
}

type shellSegment struct {
	words []shellWord
}

type shellLex struct {
	segments []shellSegment
	opaque   []string
}

var (
	plainCommandPattern = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	assignmentPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	trustedBinDirs      = map[string]bool{"/bin": true, "/usr/bin": true, "/usr/local/bin": true, "/sbin": true, "/usr/sbin": true}
	reservedWords       = map[string]bool{
		"!": true, "{": true, "}": true, "[[": true, "]]": true, "case": true, "coproc": true,
		"do": true, "done": true, "elif": true, "else": true, "esac": true, "fi": true, "for": true,
		"function": true, "if": true, "in": true, "select": true, "then": true, "time": true,
		"until": true, "while": true,
	}
)

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func lexShell(cmd string) shellLex {
	var (
		lx       shellLex
		seg      shellSegment
		cur      strings.Builder
		inWord   bool
		quoted   bool
		redirect bool
	)
	opaque := func(reason string) {
		for _, r := range lx.opaque {
			if r == reason {
				return
			}
		}
		lx.opaque = append(lx.opaque, reason)
	}
	endWord := func() {
		if inWord {
			seg.words = append(seg.words, shellWord{text: cur.String(), quoted: quoted, redirectTarget: redirect})
			redirect = false
		}
		cur.Reset()
		inWord, quoted = false, false
	}
	endSegment := func() {
		endWord()
		if redirect {
			opaque("redirection without a target")
			redirect = false
		}
		if len(seg.words) > 0 {
			lx.segments = append(lx.segments, seg)
		}
		seg = shellSegment{}
	}

	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		var next rune
		if i+1 < len(rs) {
			next = rs[i+1]
		}

		switch {
		case r == ' ' || r == '\t':
			endWord()
		case r == '\n' || r == ';' || r == '|' || r == '(' || r == ')':
			endSegment()
		case r == '&' && next == '>':
			endWord()
			i++
			for i+1 < len(rs) && rs[i+1] == '>' {
				i++
			}
			redirect = true
		case r == '&':
			endSegment()
		case r == '<' || r == '>':
			if next == '(' {
				opaque("process substitution")
			}
			if r == '<' && next == '<' {
				opaque("here-document")
			}
			if inWord && !quoted && isDigits(cur.String()) {
				cur.Reset()
				inWord = false
			} else {
				endWord()
			}
			for i+1 < len(rs) && (rs[i+1] == '>' || rs[i+1] == '<' || rs[i+1] == '|') {
				i++
			}
			if i+1 < len(rs) && rs[i+1] == '&' {
				i++
				for i+1 < len(rs) && (isDigits(string(rs[i+1])) || rs[i+1] == '-') {
					i++
				}
				continue
			}
			redirect = true
		case r == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j >= len(rs) {
				opaque("unterminated quote")
				i = len(rs)
				continue
			}
			cur.WriteString(string(rs[i+1 : j]))
			inWord, quoted = true, true
			i = j
		case r == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '$' || rs[j] == '`' || rs[j] == '\\' {
					opaque("expansion or escape inside double quotes")
				}
				j++
			}
			if j >= len(rs) {
				opaque("unterminated quote")
				i = len(rs)
				continue
			}
			cur.WriteString(string(rs[i+1 : j]))
			inWord, quoted = true, true
			i = j
		case r == '\\':
			opaque("backslash escape")
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			}
			inWord, quoted = true, true
		case r == '$':
			opaque("parameter, command or arithmetic expansion")
			cur.WriteRune(r)
			inWord = true
		case r == '`':
			opaque("command substitution")
			inWord = true
		case r == '#' && !inWord:
			opaque("comment")
			i = len(rs)
		case r < 0x20 || r == 0x7f:
			opaque("control character")
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	endSegment()

	if len(lx.segments) == 0 {
		opaque("empty command")
	}
	return lx
}

// commandName returns the program a segment runs, or "" with a reason when it is
// not a plain literal the gate can name: quoted or escaped names, assignments,
// shell keywords, and paths outside the standard binary directories.
func commandName(seg shellSegment) (string, string) {
	for _, w := range seg.words {
		if w.redirectTarget {
			continue
		}
		switch {
		case w.quoted:
			return "", "quoted or escaped command name"
		case assignmentPattern.MatchString(w.text):
			return "", "variable assignment"
		case reservedWords[w.text]:
			return "", "shell keyword " + w.text
		case strings.Contains(w.text, "/"):
			if !trustedBinDirs[path.Dir(w.text)] || !plainCommandPattern.MatchString(path.Base(w.text)) {
				return "", "command path outside the standard binary directories"
			}
			return path.Base(w.text), ""
		case plainCommandPattern.MatchString(w.text):
			return w.text, ""
		default:
			return "", "unrecognised command name"
		}
	}
	return "", "redirection without a command"
}
