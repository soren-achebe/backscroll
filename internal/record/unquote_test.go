package record

import "testing"

// Corpus generated from real emitters:
//
//	bash 5.2:  printf '%q'  (what kitty.bash sends via _ksi_get_current_command)
//	zsh 5.9:   print -f '%q' / ${(q)...}  (what kitty-integration sends)
func TestShellUnquote(t *testing.T) {
	cases := []struct{ in, want string }{
		// bash printf %q — backslash-escaped form
		{`ls\ -la`, `ls -la`},
		{`echo\ \"hi\ there\"`, `echo "hi there"`},
		{`echo\ \'single\'`, `echo 'single'`},
		{`grep\ -r\ \"foo\;bar\"\ .`, `grep -r "foo;bar" .`},
		{`echo\ \$HOME`, `echo $HOME`},
		{`printf\ \"a\\tb\\n\"`, `printf "a\tb\n"`},
		{`awk\ \'\{print\ \$1\}\'\ file`, `awk '{print $1}' file`},
		{`echo\ a\;b`, `echo a;b`},
		{`back\\slash`, `back\slash`},
		{`héllo\ wörld\ →`, `héllo wörld →`},
		// bash printf %q — whole-string $'...' form (control chars present)
		{`$'line1\nline2'`, "line1\nline2"},
		{`$'tab\there'`, "tab\there"},
		{`$'esc\x1b[31mred'`, "esc\x1b[31mred"},
		{`$'bel\a nul\x00'`, "bel\a nul\x00"},
		{`$'oct\033done'`, "oct\033done"},
		{`$'uni\u00e9'`, "uni\u00e9"},
		// zsh — mixed segments
		{`line1$'\n'line2`, "line1\nline2"},
		{`tab$'\t'here`, "tab\there"},
		{`x$'\n'y`, "x\ny"},
		{`a\ b\;c`, `a b;c`},
		// single/double quoted segments (other %q dialects)
		{`'literal $HOME'`, `literal $HOME`},
		{`"dq \$v \\ end"`, `dq $v \ end`},
		{`"dq \x kept"`, `dq \x kept`}, // \x not special in double quotes
		// best-effort on malformed input
		{``, ``},
		{`plain`, `plain`},
		{`$'unterminated`, `unterminated`},
		{`'unterminated`, `unterminated`},
		{`trailing\`, `trailing\`},
		{`$'\q'`, `\q`}, // unknown escape kept literally
	}
	for _, c := range cases {
		if got := shellUnquote(c.in); got != c.want {
			t.Errorf("shellUnquote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStripSpace(t *testing.T) {
	if got := stripSpace("YWJj\nZGVm\n"); got != "YWJjZGVm" {
		t.Errorf("stripSpace: got %q", got)
	}
	if got := stripSpace(" a\tb\r\nc "); got != "abc" {
		t.Errorf("stripSpace: got %q", got)
	}
}
