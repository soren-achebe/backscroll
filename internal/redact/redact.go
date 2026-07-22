// Package redact masks secrets (tokens, keys, passwords) in recorded
// terminal output so it can be stored or shared safely.
//
// It is pattern-based and intentionally conservative-by-overmatching: it is
// better to mask a harmless string that looks like a credential than to leak
// a real one. It is NOT a guarantee — always eyeball anything you share.
package redact

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const mask = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string // expansion template for ReplaceAll
}

// Built-in rules. Order matters: multi-line blocks and specific token
// formats run before the generic key=value rule, so already-masked text is
// not re-matched and specific formats keep their labels.
var rules = []rule{
	// Private key blocks (multi-line).
	{regexp.MustCompile(`(?s)-----BEGIN ([A-Z0-9 ]*)PRIVATE KEY( BLOCK)?-----.*?-----END ([A-Z0-9 ]*)PRIVATE KEY( BLOCK)?-----`),
		"-----BEGIN ${1}PRIVATE KEY${2}-----\n" + mask + "\n-----END ${3}PRIVATE KEY${4}-----"},

	// Credentials embedded in URLs: scheme://user:pass@host
	{regexp.MustCompile(`(://[^/\s:@]{1,64}):([^/\s@]{1,256})@`), "${1}:" + mask + "@"},

	// Authorization headers (curl -H, HTTP dumps, etc.).
	{regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?(?:bearer|basic|token)\s+)[^\s"']+`), "${1}" + mask},

	// Well-known token formats.
	{regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`), mask},                          // AWS access key id
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,255}\b`), mask},                                // GitHub tokens
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,255}\b`), mask},                              // GitHub fine-grained PAT
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,255}\b`), mask},                              // Slack
	{regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{10,255}\b`), mask},               // Stripe
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,255}\b`), mask},                                     // OpenAI / Anthropic style
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), mask},                                        // Google API key
	{regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`), mask},                                          // npm
	{regexp.MustCompile(`\bpypi-[A-Za-z0-9_-]{20,}\b`), mask},                                      // PyPI
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,255}\b`), mask},                                  // GitLab PAT
	{regexp.MustCompile(`\bdop_v1_[a-f0-9]{64}\b`), mask},                                          // DigitalOcean
	{regexp.MustCompile(`\bhf_[A-Za-z]{30,}\b`), mask},                                             // Hugging Face
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), mask},   // JWT

	// Generic key=value / key: value assignments. Keeps the key, masks the
	// value. Runs last so specific formats above win.
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|client[_-]?secret|token|auth[_-]?token|api[_-]?key|apikey|access[_-]?key|secret[_-]?key|private[_-]?key)\b(["']?\s*[=:]\s*["']?)([^\s"'&;,]{4,})`),
		"${1}${2}" + mask},
}

// String redacts s with the built-in rules plus any extra user patterns
// (whole match replaced). It returns the redacted text and the number of
// replacements made.
func String(s string, extra []*regexp.Regexp) (string, int) {
	n := 0
	for _, r := range rules {
		matches := r.re.FindAllStringIndex(s, -1)
		if len(matches) == 0 {
			continue
		}
		out := r.re.ReplaceAllString(s, r.repl)
		if out != s { // already-masked text can re-match; don't count no-ops
			n += len(matches)
			s = out
		}
	}
	for _, re := range extra {
		matches := re.FindAllStringIndex(s, -1)
		if len(matches) == 0 {
			continue
		}
		out := re.ReplaceAllString(s, mask)
		if out != s {
			n += len(matches)
			s = out
		}
	}
	return s, n
}

// Bytes is String for byte slices.
func Bytes(b []byte, extra []*regexp.Regexp) ([]byte, int) {
	s, n := String(string(b), extra)
	return []byte(s), n
}

// File returns the path of the user redact-patterns file.
func File() string {
	if p := os.Getenv("BACKSCROLL_REDACT_FILE"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "backscroll", "redact")
}

// LoadExtra reads the user patterns file: one Go regexp per line, each
// whole match replaced with [REDACTED]. Blank lines and '#' comments are
// skipped; invalid regexps are skipped (they must never break a command).
func LoadExtra() []*regexp.Regexp {
	f, err := os.Open(File())
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
