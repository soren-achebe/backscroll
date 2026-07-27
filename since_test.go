package main

import (
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	now := time.Now()
	approx := func(got time.Time, wantAgo time.Duration) bool {
		want := now.Add(-wantAgo)
		d := got.Sub(want)
		return d > -5*time.Second && d < 5*time.Second
	}
	cases := []struct {
		in  string
		ago time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"2h", 2 * time.Hour},
		{"90s", 90 * time.Second},
		{"3d", 3 * 24 * time.Hour},
		{"1.5d", 36 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
	}
	for _, c := range cases {
		got, err := parseTimeSpec(c.in)
		if err != nil {
			t.Errorf("parseTimeSpec(%q): %v", c.in, err)
			continue
		}
		if !approx(got, c.ago) {
			t.Errorf("parseTimeSpec(%q) = %v, want ~%v ago", c.in, got, c.ago)
		}
	}

	got, err := parseTimeSpec("2026-07-01")
	if err != nil {
		t.Fatalf("date: %v", err)
	}
	want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("date: got %v want %v", got, want)
	}
	got, err = parseTimeSpec("2026-07-01 13:45")
	if err != nil {
		t.Fatalf("datetime: %v", err)
	}
	want = time.Date(2026, 7, 1, 13, 45, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("datetime: got %v want %v", got, want)
	}

	for _, bad := range []string{"", "yesterday", "5x", "d", "2026-13-40"} {
		if _, err := parseTimeSpec(bad); err == nil {
			t.Errorf("parseTimeSpec(%q): expected error", bad)
		}
	}
}
