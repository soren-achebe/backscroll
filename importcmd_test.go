package main

import (
	"os"
	"path/filepath"
	"testing"
)

func cpFixture(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDetectHistSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	t.Setenv("XDG_DATA_HOME", "")
	// an exported HISTFILE must NOT leak into detection
	t.Setenv("HISTFILE", filepath.Join(home, ".bash_history"))

	if got := detectHistSources(); len(got) != 0 {
		t.Fatalf("empty home: want no sources, got %+v", got)
	}

	td := "internal/histimport/testdata"
	cpFixture(t, td+"/atuin_history.db", filepath.Join(home, ".local/share/atuin/history.db"))
	cpFixture(t, td+"/zsh_history", filepath.Join(home, ".zsh_history"))
	cpFixture(t, td+"/bash_history_ts", filepath.Join(home, ".bash_history"))
	cpFixture(t, td+"/fish_history", filepath.Join(home, ".local/share/fish/fish_history"))

	got := detectHistSources()
	if len(got) != 4 {
		t.Fatalf("want 4 sources, got %+v", got)
	}
	// order = data richness: atuin first, then zsh/bash/fish
	wantOrder := []string{"atuin", "zsh", "bash", "fish"}
	for i, h := range got {
		if h.Name != wantOrder[i] {
			t.Errorf("pos %d: want %s, got %s", i, wantOrder[i], h.Name)
		}
		if h.Entries <= 0 {
			t.Errorf("%s: want parsed entries > 0, got %d (path %s)", h.Name, h.Entries, h.Path)
		}
		if h.Path == "" {
			t.Errorf("%s: empty path", h.Name)
		}
	}

	// empty file present -> suppressed, not suggested
	if err := os.WriteFile(filepath.Join(home, ".zsh_history"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got = detectHistSources()
	for _, h := range got {
		if h.Name == "zsh" {
			t.Errorf("empty zsh_history should be suppressed, got %+v", h)
		}
	}
}
