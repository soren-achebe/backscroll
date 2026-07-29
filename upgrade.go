package main

// backscroll upgrade — explicit, checksum-verified self-update.
//
// This is the ONLY code path in backscroll that touches the network, and it
// runs only when the user types `backscroll upgrade` (or `upgrade --check`).
// There is no background update check, no telemetry, no phone-home — ever.
//
// Flow:
//  1. Resolve the latest release tag from the /releases/latest redirect
//     (no API, no rate limits, nothing sent but the request itself).
//  2. Download the release archive for this OS/arch plus checksums.txt.
//  3. Verify the archive's sha256 against checksums.txt.
//  4. Extract the binary, sanity-run `<new> version`, then atomically
//     rename it over the current executable.
//
// Managed installs (Homebrew, Scoop, deb/rpm, Nix, Docker) are detected from
// the executable path and refused with the right package-manager command —
// silently diverging from a package manager's idea of what's installed is
// how you end up with two broken installs.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Overridable in tests.
var (
	upgradeReleaseBase = "https://github.com/soren-achebe/backscroll/releases"
	upgradeMaxArchive  = int64(200 << 20) // refuse absurd downloads
	upgradeExecutable  = os.Executable
)

func cmdUpgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	check := fs.Bool("check", false, "only report whether a newer release exists (still one network request; nothing is installed)")
	pin := fs.String("version", "", "install this release tag (e.g. v0.11.2) instead of the latest; allows downgrades")
	fs.Parse(args)

	cur := resolvedVersion()
	if cur == "dev" && *pin == "" && !*check {
		return fmt.Errorf("this is a from-source build (version \"dev\"); upgrade it with git pull + go build, or force a release with --version vX.Y.Z")
	}

	client := &http.Client{Timeout: 60 * time.Second}

	target := strings.TrimPrefix(*pin, "v")
	if target == "" {
		latest, err := latestReleaseTag(client)
		if err != nil {
			return fmt.Errorf("could not determine the latest release: %w", err)
		}
		target = strings.TrimPrefix(latest, "v")
	}

	if *check {
		fmt.Printf("current: %s\nlatest:  %s\n", cur, target)
		switch {
		case cur == target:
			fmt.Println("up to date.")
		case semverLess(cur, target):
			fmt.Println("newer release available — run: backscroll upgrade")
		default:
			fmt.Println("current build is newer than the latest release.")
		}
		return nil
	}

	if cur == target {
		fmt.Printf("backscroll %s is already the latest release.\n", cur)
		return nil
	}
	if *pin == "" && !semverLess(cur, target) {
		fmt.Printf("current version %s is newer than the latest release %s; nothing to do.\n", cur, target)
		return nil
	}
	if *pin != "" && semverLess(target, cur) {
		fmt.Printf("note: downgrading %s -> %s (--version was given)\n", cur, target)
	}

	exePath, err := upgradeExecutable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	if hint := managedInstallHint(exePath); hint != "" {
		return fmt.Errorf("this install is managed by a package manager; upgrade it there instead:\n  %s", hint)
	}
	exeDir := filepath.Dir(exePath)
	if err := writableDir(exeDir); err != nil {
		return fmt.Errorf("cannot write to %s (%v)\nre-run the installer or use the method you installed with", exeDir, err)
	}
	// Clean up a leftover from a previous Windows upgrade, best-effort.
	os.Remove(exePath + ".old")

	asset := fmt.Sprintf("backscroll_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	binName := "backscroll"
	if runtime.GOOS == "windows" {
		asset = fmt.Sprintf("backscroll_windows_%s.zip", runtime.GOARCH)
		binName = "backscroll.exe"
	}
	base := fmt.Sprintf("%s/download/v%s", upgradeReleaseBase, target)

	fmt.Printf("downloading %s (v%s)...\n", asset, target)
	archive, err := fetchLimited(client, base+"/"+asset, upgradeMaxArchive)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	sums, err := fetchLimited(client, base+"/checksums.txt", 1<<20)
	if err != nil {
		return fmt.Errorf("download failed (checksums.txt): %w", err)
	}

	want, err := checksumFor(sums, asset)
	if err != nil {
		return err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("sha256 mismatch for %s\n  expected: %s\n  got:      %s\nnot installing", asset, want, hex.EncodeToString(got[:]))
	}
	fmt.Println("checksum OK.")

	bin, err := extractBinary(archive, asset, binName)
	if err != nil {
		return err
	}

	// Stage next to the target so the final rename is atomic (same fs).
	tmp, err := os.CreateTemp(exeDir, ".backscroll-upgrade-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}

	// Sanity-run the staged binary before swapping it in.
	out, err := exec.Command(tmpPath, "version").Output()
	if err != nil {
		return fmt.Errorf("downloaded binary failed to run (%v); not installing", err)
	}
	if !strings.Contains(string(out), target) {
		return fmt.Errorf("downloaded binary reports %q, expected version %s; not installing", strings.TrimSpace(string(out)), target)
	}

	if runtime.GOOS == "windows" {
		// Can't overwrite a running .exe, but renaming it away is allowed.
		if err := os.Rename(exePath, exePath+".old"); err != nil {
			return fmt.Errorf("could not move the current binary aside: %w", err)
		}
		if err := os.Rename(tmpPath, exePath); err != nil {
			os.Rename(exePath+".old", exePath) // best-effort rollback
			return err
		}
		os.Remove(exePath + ".old") // fails while still running; next upgrade cleans it
	} else {
		if err := os.Rename(tmpPath, exePath); err != nil {
			return err
		}
	}

	fmt.Printf("upgraded: %s -> %s (%s)\n", cur, target, exePath)
	return nil
}

// latestReleaseTag resolves the tag of the latest release from the
// /releases/latest redirect Location header — no GitHub API involved.
func latestReleaseTag(client *http.Client) (string, error) {
	req, err := http.NewRequest("GET", upgradeReleaseBase+"/latest", nil)
	if err != nil {
		return "", err
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if resp.StatusCode/100 != 3 || i < 0 {
		return "", fmt.Errorf("unexpected response %d from %s/latest", resp.StatusCode, upgradeReleaseBase)
	}
	tag := loc[i+len("/tag/"):]
	if tag == "" || !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("unexpected release tag %q", tag)
	}
	return tag, nil
}

func fetchLimited(client *http.Client, url string, max int64) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("response larger than %d bytes for %s", max, url)
	}
	return data, nil
}

// checksumFor finds the sha256 for name in a `sha256  filename` listing.
func checksumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			if len(f[0]) != 64 {
				return "", fmt.Errorf("malformed checksum line for %s", name)
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s not found in checksums.txt", name)
}

// extractBinary pulls binName out of a .tar.gz or .zip release archive.
func extractBinary(archive []byte, asset, binName string) ([]byte, error) {
	if strings.HasSuffix(asset, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == binName {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, upgradeMaxArchive))
			}
		}
		return nil, fmt.Errorf("%s not found in %s", binName, asset)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == binName {
			return io.ReadAll(io.LimitReader(tr, upgradeMaxArchive))
		}
	}
	return nil, fmt.Errorf("%s not found in %s", binName, asset)
}

// managedInstallHint returns the package-manager upgrade command when the
// executable path clearly belongs to one, else "".
func managedInstallHint(exePath string) string {
	// Normalize both separator styles (filepath.ToSlash is a no-op for
	// backslashes when the test runs on unix).
	p := strings.ReplaceAll(exePath, "\\", "/")
	lower := strings.ToLower(p)
	switch {
	case strings.Contains(p, "/Cellar/") || strings.Contains(p, "/Caskroom/") ||
		strings.Contains(lower, "/homebrew/") || strings.Contains(lower, "/linuxbrew/"):
		return "brew update && brew upgrade backscroll"
	case strings.Contains(lower, "/scoop/apps/"):
		return "scoop update backscroll"
	case strings.HasPrefix(p, "/nix/store/"):
		return "nix profile upgrade (or your flake/config)  # /nix/store is immutable"
	case p == "/usr/bin/backscroll":
		return "your deb/rpm package manager (download the new package from the releases page)"
	}
	return ""
}

// writableDir verifies we can create a file in dir (permission checks alone
// lie on some filesystems).
func writableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".backscroll-wtest-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// semverLess reports a < b for dotted numeric versions ("0.11.2").
// Non-numeric segments compare as strings; missing segments are 0.
func semverLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		an, aerr := strconv.Atoi(av)
		bn, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			if an != bn {
				return an < bn
			}
			continue
		}
		if av != bv {
			return av < bv
		}
	}
	return false
}
