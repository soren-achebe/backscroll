package main

import (
	"strings"
	"testing"
)

func TestFenceFor(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"plain output", "```"},
		{"has `inline` code", "```"},
		{"has ``` a fence", "````"},
		{"x``````y", "```````"},
		{"", "```"},
	}
	for _, c := range cases {
		if got := fenceFor(c.body); got != c.want {
			t.Errorf("fenceFor(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestHTMLEscape(t *testing.T) {
	if got := htmlEscape(`a < b && c > d`); got != "a &lt; b &amp;&amp; c &gt; d" {
		t.Errorf("htmlEscape = %q", got)
	}
}

func TestRound2(t *testing.T) {
	if round2(0.6149999) != 0.615 && round2(0.615) != 0.615 {
		t.Errorf("round2 broken: %v", round2(0.615))
	}
	if round2(0.0) != 0.0 {
		t.Errorf("round2(0) = %v", round2(0.0))
	}
}

func TestFenceForNeverContained(t *testing.T) {
	// property: the chosen fence must never appear in the body
	bodies := []string{
		"a", "`", "``", "```", "````", "`````",
		strings.Repeat("`", 20) + "text" + strings.Repeat("`", 7),
	}
	for _, b := range bodies {
		f := fenceFor(b)
		if strings.Contains(b, f) {
			t.Errorf("fence %q appears in body %q", f, b)
		}
	}
}
