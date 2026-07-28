package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/redact"
	"github.com/soren-achebe/backscroll/internal/store"
)

// cmdExport renders stored commands for sharing: a markdown code block you
// can paste into a GitHub issue or Slack, an asciicast v2 file you can play
// with asciinema, plain JSON for scripting, or a self-contained HTML page
// (dark theme, full ANSI color, zero JS). Commands are picked either
// by explicit ids / -N offsets, or by the shared list/search filters
// (--exit fail --since 1d → today's failures, oldest first).
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	format := fs.String("format", "md", "output format: md, cast, json, html")
	details := fs.Bool("details", false, "md: wrap in a collapsible <details> block")
	raw := fs.Bool("raw", false, "md/json: keep ANSI colors (cast always keeps them)")
	doRedact := fs.Bool("redact", false, "mask secrets (tokens, keys, passwords) before exporting")
	out := fs.String("o", "", "write to file instead of stdout")
	n := fs.Int("n", 20, "with filters: export at most the newest `n` matches")
	mkFilter := filterFlags(fs)

	// Accept ids and bare -N offsets anywhere (like show/diff): -1 = last,
	// -2 = one before, etc. splitTargets knows which flags consume a value,
	// so "--exit 1" or "-n 5" is never mistaken for a target id.
	flags, pos := splitTargets(fs, args)
	fs.Parse(flags)
	pos = append(pos, fs.Args()...)

	f, err := mkFilter(*n)
	if err != nil {
		return err
	}
	if f.Active() && len(pos) > 0 {
		return fmt.Errorf("give explicit ids or filters, not both")
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	var cmds []*store.Command
	if f.Active() {
		rows, err := st.List(f)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("no commands match those filters")
		}
		// List is newest-first; export reads best oldest-first.
		for i := len(rows) - 1; i >= 0; i-- {
			c, err := st.Get(rows[i].ID)
			if err != nil {
				return fmt.Errorf("%d: %v", rows[i].ID, err)
			}
			cmds = append(cmds, c)
		}
	} else {
		if len(pos) == 0 {
			pos = []string{"-1"} // default: last command
		}
		for _, p := range pos {
			n, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return fmt.Errorf("bad id %q (want an id or -N offset)", p)
			}
			c, err := st.Get(n)
			if err != nil {
				return fmt.Errorf("%s: not found (%v)", p, err)
			}
			cmds = append(cmds, c)
		}
	}
	if *doRedact {
		extra := redact.LoadExtra()
		for _, c := range cmds {
			c.Cmd, _ = redact.String(c.Cmd, extra)
			c.Output, _ = redact.Bytes(c.Output, extra)
			c.Note, _ = redact.String(c.Note, extra)
		}
	}

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}

	switch *format {
	case "md", "markdown":
		return exportMarkdown(w, cmds, *details, *raw)
	case "cast", "asciicast":
		return exportCast(w, cmds)
	case "json":
		return exportJSON(w, cmds, *raw)
	case "html":
		return exportHTML(w, cmds)
	default:
		return fmt.Errorf("unknown format %q (md, cast, json, html)", *format)
	}
}

// splitTargets separates positional id/-N targets from flag tokens. It must
// run before fs.Parse but after all flags are registered: when a token is a
// registered non-boolean flag without an inline =value, the following token
// is kept with it, so numeric flag values ("--exit 1", "-n 5") are never
// misread as target ids.
func splitTargets(fs *flag.FlagSet, args []string) (flags, targets []string) {
	valueFlag := func(name string) bool {
		fl := fs.Lookup(name)
		if fl == nil {
			return false
		}
		b, ok := fl.Value.(interface{ IsBoolFlag() bool })
		return !(ok && b.IsBoolFlag())
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if _, err := strconv.ParseInt(a, 10, 64); err == nil {
			targets = append(targets, a)
			continue
		}
		flags = append(flags, a)
		if strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
			if name := strings.TrimLeft(a, "-"); valueFlag(name) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	return flags, targets
}

func exportOutput(c *store.Command, raw bool) []byte {
	o := c.Output
	if !raw {
		o = ansi.Strip(o)
	}
	return o
}

// fenceFor returns a backtick fence long enough to safely wrap body even if
// the output itself contains ``` runs.
func fenceFor(body string) string {
	max := 2
	run := 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > max {
				max = run
			}
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max+1)
}

func exportMarkdown(w *os.File, cmds []*store.Command, details, raw bool) error {
	for i, c := range cmds {
		if i > 0 {
			fmt.Fprintln(w)
		}
		body := string(exportOutput(c, raw))
		body = strings.TrimRight(body, "\n")
		block := "$ " + c.Cmd
		if body != "" {
			block += "\n" + body
		}
		fence := fenceFor(block)
		meta := fmt.Sprintf("exit %s · %s · %s",
			exitStr(*c), fmtSpan(*c),
			fmtWhen(c.StartedAt))
		if c.Truncated {
			meta += " · output truncated"
		}
		if c.Note != "" {
			meta += " · note: " + oneLine(c.Note, 120)
		}
		if details {
			fmt.Fprintf(w, "<details>\n<summary><code>%s</code> — %s</summary>\n\n",
				htmlEscape(oneLine(c.Cmd, 90)), meta)
		}
		fmt.Fprintf(w, "%sconsole\n%s\n%s\n", fence, block, fence)
		if details {
			fmt.Fprint(w, "\n</details>\n")
		} else {
			fmt.Fprintf(w, "<sub>%s</sub>\n", meta)
		}
	}
	return nil
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// exportCast writes an asciicast v2 file (play with `asciinema play f.cast`).
// We don't store per-byte timing, so playback is paced per line: the prompt
// and command appear first, then the output flows in quickly.
func exportCast(w *os.File, cmds []*store.Command) error {
	type header struct {
		Version   int    `json:"version"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Timestamp int64  `json:"timestamp"`
		Title     string `json:"title,omitempty"`
	}
	title := oneLine(cmds[0].Cmd, 60)
	if len(cmds) > 1 {
		title += fmt.Sprintf(" (+%d more)", len(cmds)-1)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(header{
		Version: 2, Width: 100, Height: 30,
		Timestamp: cmds[0].StartedAt.Unix(), Title: title,
	}); err != nil {
		return err
	}
	t := 0.0
	event := func(data string) error {
		return enc.Encode([]any{round2(t), "o", data})
	}
	for _, c := range cmds {
		if err := event("\x1b[1;32m$\x1b[0m \x1b[1m" + c.Cmd + "\x1b[0m\r\n"); err != nil {
			return err
		}
		t += 0.6
		// PTY output already uses \r\n; stored raw output keeps it. Pace
		// per line so replay is watchable but fast.
		body := string(c.Output)
		for len(body) > 0 {
			i := strings.IndexByte(body, '\n')
			var line string
			if i < 0 {
				line, body = body, ""
			} else {
				line, body = body[:i+1], body[i+1:]
			}
			if !strings.HasSuffix(line, "\r\n") {
				line = strings.TrimRight(line, "\n") + "\r\n"
			}
			if err := event(line); err != nil {
				return err
			}
			t += 0.015
		}
		t += 0.8
	}
	return nil
}

func round2(f float64) float64 {
	return float64(int(f*1000+0.5)) / 1000
}

func exportJSON(w *os.File, cmds []*store.Command, raw bool) error {
	type rec struct {
		ID         int64  `json:"id"`
		Cmd        string `json:"cmd"`
		Cwd        string `json:"cwd"`
		ExitCode   *int64 `json:"exit_code"` // null if unknown
		StartedAt  string `json:"started_at"`
		EndedAt    string `json:"ended_at"`
		DurationMS int64  `json:"duration_ms"`
		Truncated  bool   `json:"truncated"`
		Note       string `json:"note,omitempty"`
		Output     string `json:"output"`
	}
	recs := make([]rec, 0, len(cmds))
	for _, c := range cmds {
		r := rec{
			ID: c.ID, Cmd: c.Cmd, Cwd: c.Cwd,
			StartedAt:  c.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
			EndedAt:    c.EndedAt.Format("2006-01-02T15:04:05Z07:00"),
			DurationMS: c.EndedAt.Sub(c.StartedAt).Milliseconds(),
			Truncated:  c.Truncated,
			Note:       c.Note,
			Output:     string(exportOutput(c, raw)),
		}
		if c.ExitCode.Valid {
			v := c.ExitCode.Int64
			r.ExitCode = &v
		}
		recs = append(recs, r)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(recs)
}
