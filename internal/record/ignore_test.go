package record

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnore(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "ignore")
	os.WriteFile(f, []byte("# comment\n\n^vault \npassword\n(bad[regex\n"), 0o644)
	t.Setenv("BACKSCROLL_IGNORE_FILE", f)
	pats := LoadIgnore()
	if len(pats) != 2 {
		t.Fatalf("want 2 valid patterns, got %d", len(pats))
	}
	cases := map[string]bool{
		"vault kv get secret/x": true,
		"echo mypassword123":    true,
		"ls -la":                false,
		"grep vault notes.md":   false,
	}
	for cmd, want := range cases {
		if got := Ignored(pats, cmd); got != want {
			t.Errorf("Ignored(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestLoadIgnoreMissingFile(t *testing.T) {
	t.Setenv("BACKSCROLL_IGNORE_FILE", "/nonexistent/nope")
	if pats := LoadIgnore(); pats != nil {
		t.Fatalf("want nil, got %v", pats)
	}
}
