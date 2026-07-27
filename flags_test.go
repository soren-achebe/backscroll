package main

import (
	"flag"
	"reflect"
	"testing"
)

func splitFS() *flag.FlagSet {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("since", "", "")
	fs.String("until", "", "")
	fs.Int("C", -1, "")
	fs.Bool("redact", false, "")
	return fs
}

func TestSplitFlags(t *testing.T) {
	cases := []struct {
		in        []string
		wantFlags []string
		wantPos   []string
	}{
		// trailing filters after the query
		{[]string{"boom", "--since", "1d"}, []string{"--since", "1d"}, []string{"boom"}},
		// flags-first still works
		{[]string{"--since", "1d", "boom"}, []string{"--since", "1d"}, []string{"boom"}},
		// bool flag takes no value
		{[]string{"boom", "--redact", "town"}, []string{"--redact"}, []string{"boom", "town"}},
		// = form
		{[]string{"boom", "--since=1d"}, []string{"--since=1d"}, []string{"boom"}},
		// value flag with numeric value that could look positional
		{[]string{"err", "-C", "2", "--until", "1h"}, []string{"-C", "2", "--until", "1h"}, []string{"err"}},
		// unregistered dash-word stays in the query
		{[]string{"--not-a-flag", "boom"}, nil, []string{"--not-a-flag", "boom"}},
		// bare -- ends flag parsing
		{[]string{"--since", "1d", "--", "--redact"}, []string{"--since", "1d"}, []string{"--redact"}},
		// lone dash is positional
		{[]string{"-"}, nil, []string{"-"}},
	}
	for _, c := range cases {
		flags, pos := splitFlags(splitFS(), c.in)
		if !reflect.DeepEqual(flags, c.wantFlags) || !reflect.DeepEqual(pos, c.wantPos) {
			t.Errorf("splitFlags(%v) = %v / %v, want %v / %v",
				c.in, flags, pos, c.wantFlags, c.wantPos)
		}
	}
}
