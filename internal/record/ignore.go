package record

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// IgnoreFile returns the path of the ignore-patterns file.
func IgnoreFile() string {
	if p := os.Getenv("BACKSCROLL_IGNORE_FILE"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "backscroll", "ignore")
}

// LoadIgnore reads the ignore file: one Go regexp per line, matched
// (unanchored) against the full command text. Blank lines and lines
// starting with '#' are skipped. Invalid regexps are skipped too — a
// bad line must never break recording.
func LoadIgnore() []*regexp.Regexp {
	f, err := os.Open(IgnoreFile())
	if err != nil {
		return nil
	}
	defer f.Close()
	var pats []*regexp.Regexp
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if re, err := regexp.Compile(line); err == nil {
			pats = append(pats, re)
		}
	}
	return pats
}

// Ignored reports whether cmd matches any ignore pattern.
func Ignored(pats []*regexp.Regexp, cmd string) bool {
	for _, re := range pats {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}
