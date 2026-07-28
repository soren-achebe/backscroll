package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/soren-achebe/backscroll/internal/ansihtml"
	"github.com/soren-achebe/backscroll/internal/store"
)

// exportHTML writes a single self-contained HTML page: no external
// assets, no JavaScript, dark theme, output rendered with full ANSI
// color (16/256/truecolor). Made for sharing — attach it to a ticket,
// drop it in a paste site, or open it locally. Colors are always kept
// (that is the point of the format), so --raw is a no-op here.
func exportHTML(w io.Writer, cmds []*store.Command) error {
	title := oneLine(cmds[0].Cmd, 60)
	if len(cmds) > 1 {
		title += fmt.Sprintf(" (+%d more)", len(cmds)-1)
	}

	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\n")
	fmt.Fprintf(&b, "<title>%s — backscroll</title>\n", htmlEscape(title))
	b.WriteString("<style>\n:root{--bg:#0d1117;--fg:#e6edf3;--panel:#161b22;--border:#30363d;--dim:#8b949e;\n--ok:#3fb950;--bad:#f85149;\n")
	b.WriteString(ansihtml.Palette)
	b.WriteString("}\nbody{margin:0;background:var(--bg);color:var(--fg);\nfont:13px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;}\n")
	b.WriteString("main{max-width:980px;margin:0 auto;padding:24px 16px;}\n")
	b.WriteString("section{border:1px solid var(--border);border-radius:8px;background:var(--panel);\nmargin-bottom:20px;overflow:hidden;}\n")
	b.WriteString(".hdr{padding:10px 14px 0;font-weight:700;white-space:pre-wrap;word-break:break-word;}\n")
	b.WriteString(".hdr .p{color:var(--ok);user-select:none;}\n")
	b.WriteString(".meta{padding:4px 14px 10px;color:var(--dim);font-size:12px;border-bottom:1px solid var(--border);}\n")
	b.WriteString(".note{padding:6px 14px 0;color:#d4b106;font-size:12px;}\n")
	b.WriteString(".meta .ok{color:var(--ok)} .meta .bad{color:var(--bad)}\n")
	b.WriteString("pre{margin:0;padding:12px 14px;overflow-x:auto;tab-size:8;}\n")
	b.WriteString("footer{max-width:980px;margin:0 auto;padding:0 16px 24px;color:var(--dim);font-size:12px;}\n")
	b.WriteString("footer a{color:inherit;}\n")
	b.WriteString(ansihtml.CSS)
	b.WriteString("\n</style>\n</head>\n<body>\n<main>\n")

	for _, c := range cmds {
		b.WriteString("<section>\n")
		fmt.Fprintf(&b, "<div class=\"hdr\"><span class=\"p\">$</span> %s</div>\n", htmlEscape(c.Cmd))

		cls := "ok"
		if c.ExitCode.Valid && c.ExitCode.Int64 != 0 {
			cls = "bad"
		}
		meta := []string{
			fmt.Sprintf("<span class=\"%s\">exit %s</span>", cls, htmlEscape(exitStr(*c))),
			htmlEscape(fmtSpan(*c)),
			htmlEscape(fmtWhenZone(c.StartedAt)),
		}
		if c.Cwd != "" {
			meta = append(meta, htmlEscape(c.Cwd))
		}
		if c.Host != "" {
			meta = append(meta, htmlEscape("["+c.Host+"]"))
		}
		if c.Truncated {
			meta = append(meta, "output truncated")
		}
		fmt.Fprintf(&b, "<div class=\"meta\">%s</div>\n", strings.Join(meta, " · "))
		if c.Note != "" {
			fmt.Fprintf(&b, "<div class=\"note\">\u270e %s</div>\n", htmlEscape(c.Note))
		}

		out := strings.TrimRight(ansihtml.Render(c.Output), "\n")
		if out == "" {
			out = "<span class=\"di\">(no output)</span>"
		}
		fmt.Fprintf(&b, "<pre>%s</pre>\n</section>\n", out)
	}

	fmt.Fprintf(&b, "</main>\n<footer>exported with <a href=\"https://github.com/soren-achebe/backscroll\">backscroll</a> %s</footer>\n</body>\n</html>\n",
		htmlEscape(version))
	_, err := io.WriteString(w, b.String())
	return err
}

// fmtWhenZone is fmtWhen with the timezone kept — a shared HTML page
// travels across machines, so the zone matters there.
func fmtWhenZone(t time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return "time unknown"
	}
	return t.Format("2006-01-02 15:04:05 MST")
}
