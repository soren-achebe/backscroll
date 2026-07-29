package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.11.1", "0.11.2", true},
		{"0.11.2", "0.11.2", false},
		{"0.11.2", "0.11.1", false},
		{"0.9.3", "0.10.0", true}, // numeric, not lexicographic
		{"0.11", "0.11.2", true},  // missing segment = 0-ish
		{"0.11.2", "1.0.0", true},
		{"1.0.0", "0.99.99", false},
		{"0.11.2-rc1", "0.11.2-rc2", true}, // non-numeric falls back to string
	}
	for _, c := range cases {
		if got := semverLess(c.a, c.b); got != c.want {
			t.Errorf("semverLess(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestManagedInstallHint(t *testing.T) {
	cases := []struct {
		path string
		want string // substring of the hint, or "" for unmanaged
	}{
		{"/home/u/.local/bin/backscroll", ""},
		{"/usr/local/bin/backscroll", ""},
		{"/opt/homebrew/bin/backscroll", "brew"},
		{"/usr/local/Cellar/backscroll/0.11.2/bin/backscroll", "brew"},
		{"/home/linuxbrew/.linuxbrew/bin/backscroll", "brew"},
		{"C:\\Users\\u\\scoop\\apps\\backscroll\\current\\backscroll.exe", "scoop"},
		{"/nix/store/abc123-backscroll-0.11.2/bin/backscroll", "nix"},
		{"/usr/bin/backscroll", "deb/rpm"},
	}
	for _, c := range cases {
		got := managedInstallHint(c.path)
		if c.want == "" && got != "" {
			t.Errorf("managedInstallHint(%q) = %q, want unmanaged", c.path, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("managedInstallHint(%q) = %q, want mention of %q", c.path, got, c.want)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	sums := []byte("aaaa  other.tar.gz\n" +
		strings.Repeat("ab", 32) + "  backscroll_linux_amd64.tar.gz\n")
	got, err := checksumFor(sums, "backscroll_linux_amd64.tar.gz")
	if err != nil || got != strings.Repeat("ab", 32) {
		t.Fatalf("checksumFor = %q, %v", got, err)
	}
	if _, err := checksumFor(sums, "missing.tar.gz"); err == nil {
		t.Fatal("expected error for missing asset")
	}
	if _, err := checksumFor(sums, "other.tar.gz"); err == nil {
		t.Fatal("expected error for malformed (short) checksum")
	}
}

func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	tgz := makeTarGz(t, "backscroll", []byte("BIN"))
	got, err := extractBinary(tgz, "x.tar.gz", "backscroll")
	if err != nil || string(got) != "BIN" {
		t.Fatalf("tar.gz extract = %q, %v", got, err)
	}
	if _, err := extractBinary(tgz, "x.tar.gz", "nope"); err == nil {
		t.Fatal("expected error for missing member")
	}

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.Create("backscroll.exe")
	f.Write([]byte("WINBIN"))
	zw.Close()
	got, err = extractBinary(zbuf.Bytes(), "x.zip", "backscroll.exe")
	if err != nil || string(got) != "WINBIN" {
		t.Fatalf("zip extract = %q, %v", got, err)
	}
}

// fakeRelease serves a minimal GitHub-releases-shaped tree:
//
//	/latest                     -> 302 to /tag/<tag>
//	/download/<tag>/<asset>     -> archive bytes
//	/download/<tag>/checksums.txt
func fakeRelease(t *testing.T, tag string, assets map[string][]byte) *httptest.Server {
	t.Helper()
	var sums bytes.Buffer
	for name, data := range assets {
		h := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), name)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://ignored.example/releases/tag/"+tag)
		w.WriteHeader(302)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/download/"), "/", 2)
		if len(parts) != 2 || parts[0] != tag {
			http.NotFound(w, r)
			return
		}
		if parts[1] == "checksums.txt" {
			w.Write(sums.Bytes())
			return
		}
		if data, ok := assets[parts[1]]; ok {
			w.Write(data)
			return
		}
		http.NotFound(w, r)
	})
	return httptest.NewServer(mux)
}

// buildStub compiles a tiny binary that prints "backscroll <v>" for `version`.
func buildStub(t *testing.T, dir, v string) string {
	t.Helper()
	src := filepath.Join(dir, "stub.go")
	code := fmt.Sprintf(`package main
import ("fmt";"os")
func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" { fmt.Println("backscroll %s") }
}`, v)
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "stub-"+v)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build stub: %v\n%s", err, b)
	}
	os.Remove(src)
	return out
}

func TestLatestReleaseTag(t *testing.T) {
	srv := fakeRelease(t, "v0.99.0", nil)
	defer srv.Close()
	old := upgradeReleaseBase
	upgradeReleaseBase = srv.URL
	defer func() { upgradeReleaseBase = old }()

	tag, err := latestReleaseTag(&http.Client{})
	if err != nil || tag != "v0.99.0" {
		t.Fatalf("latestReleaseTag = %q, %v", tag, err)
	}
}

// TestUpgradeEndToEnd runs the full flow against the fake server: the
// "current executable" is a stub binary reporting an old version, the fake
// release serves a newer stub, and after cmdUpgrade the file at the original
// path must be the new binary.
func TestUpgradeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	dir := t.TempDir()

	oldBin := buildStub(t, dir, "0.1.0")
	exePath := filepath.Join(dir, "backscroll")
	binName := "backscroll"
	if runtime.GOOS == "windows" {
		exePath += ".exe"
		binName += ".exe"
	}
	if err := os.Rename(oldBin, exePath); err != nil {
		t.Fatal(err)
	}

	newBin := buildStub(t, dir, "0.2.0")
	newData, err := os.ReadFile(newBin)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(newBin)

	asset := fmt.Sprintf("backscroll_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := makeTarGz(t, binName, newData)
	if runtime.GOOS == "windows" {
		asset = fmt.Sprintf("backscroll_windows_%s.zip", runtime.GOARCH)
		var zbuf bytes.Buffer
		zw := zip.NewWriter(&zbuf)
		f, _ := zw.Create(binName)
		f.Write(newData)
		zw.Close()
		archive = zbuf.Bytes()
	}

	srv := fakeRelease(t, "v0.2.0", map[string][]byte{asset: archive})
	defer srv.Close()

	// cmdUpgrade resolves os.Executable() — the test binary — so we drive
	// the flow through a subprocess-free seam instead: temporarily point
	// version + executable resolution at our stub via the exported hooks.
	oldBase := upgradeReleaseBase
	upgradeReleaseBase = srv.URL
	defer func() { upgradeReleaseBase = oldBase }()
	oldVer := version
	version = "0.1.0"
	defer func() { version = oldVer }()
	oldExe := upgradeExecutable
	upgradeExecutable = func() (string, error) { return exePath, nil }
	defer func() { upgradeExecutable = oldExe }()

	if err := cmdUpgrade(nil); err != nil {
		t.Fatalf("cmdUpgrade: %v", err)
	}

	out, err := exec.Command(exePath, "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "0.2.0") {
		t.Fatalf("binary after upgrade reports %q, want 0.2.0", out)
	}

	// Corrupt-checksum path: checksums.txt describes the good archive but
	// the asset endpoint serves different bytes.
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://x/releases/tag/v0.3.0")
		w.WriteHeader(302)
	})
	var sums bytes.Buffer
	h := sha256.Sum256(archive)
	fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), asset)
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			w.Write(sums.Bytes())
			return
		}
		w.Write([]byte("EVIL BYTES NOT MATCHING"))
	})
	evil := httptest.NewServer(mux)
	defer evil.Close()
	upgradeReleaseBase = evil.URL
	version = "0.2.0"
	err = cmdUpgrade(nil)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("tampered archive: err = %v, want sha256 mismatch", err)
	}
	// And the binary must be untouched.
	out, _ = exec.Command(exePath, "version").Output()
	if !strings.Contains(string(out), "0.2.0") {
		t.Fatalf("binary changed after failed upgrade: %q", out)
	}
}

func TestUpgradeRefusesManaged(t *testing.T) {
	srv := fakeRelease(t, "v9.9.9", nil)
	defer srv.Close()
	oldBase := upgradeReleaseBase
	upgradeReleaseBase = srv.URL
	defer func() { upgradeReleaseBase = oldBase }()
	oldVer := version
	version = "0.1.0"
	defer func() { version = oldVer }()
	oldExe := upgradeExecutable
	upgradeExecutable = func() (string, error) { return "/opt/homebrew/bin/backscroll", nil }
	defer func() { upgradeExecutable = oldExe }()

	err := cmdUpgrade(nil)
	if err == nil || !strings.Contains(err.Error(), "brew upgrade") {
		t.Fatalf("managed install: err = %v, want brew hint", err)
	}
}
