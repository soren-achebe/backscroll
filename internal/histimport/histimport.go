// Package histimport parses existing shell-history stores — atuin's
// SQLite database and plain zsh/bash/fish history files — into entries
// that can seed a backscroll database. Formats were pinned against the
// real tools (atuin 18.17, zsh 5.9, fish 4.8, bash 5.2); see the tests
// for byte-level fixtures.
//
// Imported entries have no recorded output (nobody was recording at the
// time); they still make list/search/pick/stats useful from day one.
package histimport

import (
	"bufio"
	"bytes"
	"database/sql"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Entry is one command parsed from an existing history store.
type Entry struct {
	Seq     int64 // stable per-source sequence for idempotent re-import
	Cmd     string
	Cwd     string
	Host    string // origin hostname when the source records one (atuin)
	Exit    int
	HasExit bool
	Started time.Time // zero when the source has no timestamps
	Ended   time.Time
}

// seqFor returns a stable sequence number for a timestamped entry:
// unix-seconds*1000 plus a per-second collision counter. Entries keep
// the same seq across re-imports even if the file is later trimmed from
// the front, so (machine, seq) dedup keeps re-import a no-op.
type seqCounter map[int64]int64

func (sc seqCounter) seqFor(ts time.Time) int64 {
	s := ts.Unix()
	n := sc[s]
	sc[s] = n + 1
	if n > 999 { // pathological: >1000 entries in one second
		n = 999
	}
	return s*1000 + n
}

// hashSeq is the fallback for sources without timestamps (plain bash
// history): a positive FNV-1a of the command text. Identical commands
// collapse into one entry — for output-less seed data that is a
// feature (deduplicated pick list) as well as the only stable key.
func hashSeq(cmd string) int64 {
	h := fnv.New64a()
	h.Write([]byte(cmd))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// unmetafy reverses zsh's history-file metafication: 0x83 (Meta)
// followed by byte b encodes b^0x20.
func unmetafy(b []byte) []byte {
	if bytes.IndexByte(b, 0x83) < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == 0x83 && i+1 < len(b) {
			i++
			out = append(out, b[i]^0x20)
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// ParseZsh parses a zsh history file. With EXTENDED_HISTORY entries look
// like `: <start>:<elapsed>;<cmd>`; without it each line is just the
// command. Multiline commands embed `\`+newline (a backslash as the last
// byte of the line marks continuation). Bytes >= 0x80 may be metafied.
func ParseZsh(data []byte) []Entry {
	var out []Entry
	sc := seqCounter{}
	lines := bytes.Split(data, []byte("\n"))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// join continuation lines: trailing backslash eats the newline
		for bytes.HasSuffix(line, []byte(`\`)) && i+1 < len(lines) {
			i++
			line = append(append(line[:len(line)-1:len(line)-1], '\n'), lines[i]...)
		}
		line = unmetafy(line)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		e := Entry{Cmd: string(line)}
		if s, ok := strings.CutPrefix(e.Cmd, ": "); ok {
			// : 1785174961:0;echo hi
			if ts, rest, ok := strings.Cut(s, ":"); ok {
				if dur, cmd, ok := strings.Cut(rest, ";"); ok {
					sec, err1 := strconv.ParseInt(ts, 10, 64)
					el, err2 := strconv.ParseInt(dur, 10, 64)
					if err1 == nil && err2 == nil && cmd != "" {
						e.Cmd = cmd
						e.Started = time.Unix(sec, 0)
						e.Ended = e.Started.Add(time.Duration(el) * time.Second)
					}
				}
			}
		}
		if e.Cmd == "" {
			continue
		}
		if !e.Started.IsZero() {
			e.Seq = sc.seqFor(e.Started)
		} else {
			e.Seq = hashSeq(e.Cmd)
		}
		out = append(out, e)
	}
	return out
}

// ParseBash parses a bash history file. When HISTTIMEFORMAT was set,
// bash writes a `#<epoch>` line before each entry. Multiline entries
// (lithist) sit under one marker as raw lines — but sessions run
// *without* HISTTIMEFORMAT append plain un-markered lines to the same
// file, so "everything until the next marker" would glom whole later
// sessions into one entry (observed on a real mixed file). Instead a
// line continues the previous entry only while the joined text is
// syntactically incomplete: unbalanced quotes or a trailing backslash.
// Plain lines that follow a complete entry are their own entries with
// no timestamp. A command that is itself a plausible-epoch comment is
// indistinguishable from a marker — the standard importer ambiguity.
func ParseBash(data []byte) []Entry {
	var out []Entry
	sc := seqCounter{}
	var cur []string
	var curTS, pendingTS time.Time
	flush := func() {
		if len(cur) == 0 {
			return
		}
		e := Entry{Cmd: strings.Join(cur, "\n"), Started: curTS}
		if !curTS.IsZero() {
			e.Seq = sc.seqFor(curTS)
		} else {
			e.Seq = hashSeq(e.Cmd)
		}
		out = append(out, e)
		cur = nil
	}
	sc2 := bufio.NewScanner(bytes.NewReader(data))
	sc2.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc2.Scan() {
		line := sc2.Text()
		if ts, ok := bashTimestamp(line); ok {
			flush()
			pendingTS = ts
			continue
		}
		if len(cur) > 0 && needsCont(strings.Join(cur, "\n")) {
			cur = append(cur, line)
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		flush()
		cur = []string{line}
		curTS, pendingTS = pendingTS, time.Time{}
	}
	flush()
	return out
}

// needsCont reports whether s ends mid-construct: inside an unbalanced
// single or double quote, or with an unescaped trailing backslash.
func needsCont(s string) bool {
	inS, inD, esc := false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		switch {
		case c == '\\' && !inS:
			esc = true
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		}
	}
	return inS || inD || esc
}

// bashTimestamp reports whether line is a bash history time marker:
// '#' followed by only digits, in a plausible epoch range (2001-2286).
func bashTimestamp(line string) (time.Time, bool) {
	s, ok := strings.CutPrefix(line, "#")
	if !ok || len(s) < 10 || len(s) > 12 {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1_000_000_000 || n > 9_999_999_999 {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// ParseFish parses ~/.local/share/fish/fish_history: yaml-ish blocks of
// `- cmd: <escaped>` / `  when: <epoch>` with optional `  paths:` lists.
// fish escapes backslash as `\\` and newline as `\n` in cmd values.
func ParseFish(data []byte) []Entry {
	var out []Entry
	sc := seqCounter{}
	var cur *Entry
	flush := func() {
		if cur == nil || cur.Cmd == "" {
			cur = nil
			return
		}
		if !cur.Started.IsZero() {
			cur.Seq = sc.seqFor(cur.Started)
		} else {
			cur.Seq = hashSeq(cur.Cmd)
		}
		out = append(out, *cur)
		cur = nil
	}
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scan.Scan() {
		line := scan.Text()
		if v, ok := strings.CutPrefix(line, "- cmd: "); ok {
			flush()
			cur = &Entry{Cmd: fishUnescape(v)}
			continue
		}
		if cur == nil {
			continue
		}
		if v, ok := strings.CutPrefix(line, "  when: "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				cur.Started = time.Unix(n, 0)
			}
		}
		// "  paths:" and its "    - ..." items are ignored
	}
	flush()
	return out
}

func fishUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case '\\':
				b.WriteByte('\\')
			default: // unknown escape: keep verbatim
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ReadAtuin reads entries from an atuin history.db (schema pinned
// against atuin 18.17: timestamp/duration in nanoseconds, hostname
// "host:user", soft deletes via deleted_at). rowid is the seq.
func ReadAtuin(path string) ([]Entry, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT rowid, command, timestamp, duration, exit, cwd, hostname
		FROM history WHERE deleted_at IS NULL ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("read atuin db: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var tsNano, durNano int64
		var host string
		if err := rows.Scan(&e.Seq, &e.Cmd, &tsNano, &durNano, &e.Exit, &e.Cwd, &host); err != nil {
			return nil, err
		}
		if e.Cmd == "" {
			continue
		}
		if e.Exit >= 0 {
			e.HasExit = true
		}
		if h, _, ok := strings.Cut(host, ":"); ok { // "host:user"
			e.Host = h
		} else {
			e.Host = host
		}
		e.Started = time.Unix(0, tsNano)
		e.Ended = e.Started.Add(time.Duration(durNano))
		if e.Cwd == "unknown" {
			e.Cwd = ""
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
