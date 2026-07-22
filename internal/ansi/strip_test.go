package ansi

import "testing"

func TestStrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"\x1b[1;31mred\x1b[0m", "red"},
		{"line1\r\nline2\n", "line1\nline2\n"},
		{"progress 10%\rprogress 50%\rdone\n", "done\n"},
		{"\x1b]0;title\x07text", "text"},
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"tab\there", "tab here"},
		{"ab\x08c", "ac"},
		{"\x1b[2J\x1b[Hcleared", "cleared"},
	}
	for _, c := range cases {
		if got := string(Strip([]byte(c.in))); got != c.want {
			t.Errorf("Strip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
