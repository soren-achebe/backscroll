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

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"500M", 500 << 20, true},
		{"2G", 2 << 30, true},
		{"1.5g", 3 << 29, true},
		{"300000", 300000, true},
		{"64K", 64 << 10, true},
		{"1T", 1 << 40, true},
		{"500MB", 500 << 20, true},
		{"2GiB", 2 << 30, true},
		{" 10m ", 10 << 20, true},
		{"", 0, false},
		{"abc", 0, false},
		{"-5M", 0, false},
		{"M", 0, false},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if c.ok != (err == nil) {
			t.Errorf("parseSize(%q) err = %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
