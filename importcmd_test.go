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
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
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
	cpFixture(t, td+"/nu_history.sqlite3", filepath.Join(home, ".config/nushell/history.sqlite3"))
	cpFixture(t, td+"/pwsh_history", filepath.Join(home, ".local/share/powershell/PSReadLine/ConsoleHost_history.txt"))

	got := detectHistSources()
	if len(got) != 6 {
		t.Fatalf("want 6 sources, got %+v", got)
	}
	// order = data richness: atuin+nu carry exits/cwd, then the files
	wantOrder := []string{"atuin", "nu", "zsh", "bash", "fish", "pwsh"}
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

func TestNuSqliteSniffBeatsName(t *testing.T) {
	// explicit path: parser choice is by content, not extension
	home := t.TempDir()
	misnamed := filepath.Join(home, "history.txt") // sqlite bytes, txt name
	cpFixture(t, "internal/histimport/testdata/nu_history.sqlite3", misnamed)
	if !isSQLite(misnamed) {
		t.Fatal("sqlite magic not detected")
	}
	plain := filepath.Join(home, "history.sqlite3") // txt bytes, sqlite name
	cpFixture(t, "internal/histimport/testdata/nu_history.txt", plain)
	if isSQLite(plain) {
		t.Fatal("plaintext misdetected as sqlite")
	}
}
