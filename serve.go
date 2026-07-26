package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/ansihtml"
	"github.com/soren-achebe/backscroll/internal/diff"
	"github.com/soren-achebe/backscroll/internal/redact"
	"github.com/soren-achebe/backscroll/internal/store"
)

//go:embed web/index.html
var webFS embed.FS

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:4133", "listen address (loopback strongly recommended)")
	doRedact := fs.Bool("redact", false, "mask secrets in everything served (colors are lost on redacted output)")
	fs.Parse(args)

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	srv := &webServer{st: st, redact: *doRedact}
	if *doRedact {
		srv.extra = redact.LoadExtra()
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(ln.Addr().String())
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		fmt.Fprintf(os.Stderr, "\x1b[1;33mwarning:\x1b[0m listening on non-loopback %s — your entire command history is readable by anyone who can reach it\n", ln.Addr())
	} else {
		srv.checkHost = true // DNS-rebinding guard only makes sense on loopback
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.index)
	mux.HandleFunc("/api/commands", srv.commands)
	mux.HandleFunc("/api/cmd/", srv.command)
	mux.HandleFunc("/api/diff/", srv.diff)
	mux.HandleFunc("/api/stats", srv.stats)

	fmt.Printf("backscroll web UI: \x1b[1mhttp://%s/\x1b[0m  (Ctrl-C to stop; read-only)\n", ln.Addr())
	s := &http.Server{Handler: srv.guard(mux), ReadHeaderTimeout: 5 * time.Second}
	return s.Serve(ln)
}

type webServer struct {
	st        *store.Store
	redact    bool
	extra     []*regexp.Regexp
	checkHost bool
}

// guard enforces read-only methods and, on loopback, that the Host
// header is a loopback name — otherwise a malicious website could read
// the history via DNS rebinding.
func (s *webServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only server", http.StatusMethodNotAllowed)
			return
		}
		if s.checkHost && !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *webServer) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := webFS.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

type cmdJSON struct {
	ID        int64  `json:"id"`
	Cmd       string `json:"cmd"`
	Cwd       string `json:"cwd"`
	Exit      *int64 `json:"exit"`       // null = still unknown (session died)
	StartedAt int64  `json:"started_at"` // unix ms, 0 = unknown
	DurMs     int64  `json:"dur_ms"`     // -1 = unknown
	Bytes     int64  `json:"bytes"`
	Truncated bool   `json:"truncated"`
	Host      string `json:"host,omitempty"` // synced origin; "" = local
	Session   int64  `json:"session"`
	Snippet   string `json:"snippet,omitempty"` // HTML with <mark>
}

func (s *webServer) toJSON(c store.Command) cmdJSON {
	j := cmdJSON{
		ID: c.ID, Cmd: c.Cmd, Cwd: c.Cwd, Bytes: c.OutputLen,
		Truncated: c.Truncated, Host: c.Host, Session: c.SessionID, DurMs: -1,
	}
	if s.redact {
		j.Cmd, _ = redact.String(j.Cmd, s.extra)
	}
	if c.ExitCode.Valid {
		v := c.ExitCode.Int64
		j.Exit = &v
	}
	if !c.StartedAt.IsZero() {
		j.StartedAt = c.StartedAt.UnixMilli()
		if !c.EndedAt.IsZero() {
			j.DurMs = c.EndedAt.Sub(c.StartedAt).Milliseconds()
		}
	}
	return j
}

// snippetHTML converts the ANSI-highlighted FTS snippet to HTML with
// <mark> tags: swap the escapes for sentinels, redact if asked, escape,
// then restore as tags. The snippet text comes from the plain (already
// ANSI-stripped) FTS column, so after swapping the two highlight
// escapes that snippet() itself inserted, no other escapes remain.
func (s *webServer) snippetHTML(snip string) string {
	const so, se = "\x00S\x00", "\x00E\x00"
	snip = strings.ReplaceAll(snip, "\x1b[1;31m", so)
	snip = strings.ReplaceAll(snip, "\x1b[0m", se)
	if s.redact {
		// remove sentinels around a potential secret before matching,
		// mirroring cmdSearch's escape-splitting fix
		clean := strings.ReplaceAll(strings.ReplaceAll(snip, so, ""), se, "")
		red, n := redact.String(clean, s.extra)
		if n > 0 {
			snip = red // highlights lost on redacted snippets — fine
		}
	}
	esc := htmlEscape(snip)
	esc = strings.ReplaceAll(esc, so, "<mark>")
	esc = strings.ReplaceAll(esc, se, "</mark>")
	return esc
}

func (s *webServer) commands(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{Limit: 50}
	if n, err := strconv.Atoi(q.Get("n")); err == nil && n > 0 && n <= 500 {
		f.Limit = n
	}
	if v := q.Get("session"); v != "" {
		f.Session, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := q.Get("cwd"); v != "" {
		f.Cwd = v
	}
	if v := q.Get("host"); v != "" {
		f.Host = v
	}
	switch v := q.Get("exit"); {
	case v == "fail":
		f.Failed = true
	case v != "":
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.Exit, f.ExitSet = n, true
		}
	}
	if v := q.Get("since"); v != "" {
		if t, err := parseSince(v); err == nil {
			f.Since = t
		}
	}

	var out []cmdJSON
	if qs := strings.TrimSpace(q.Get("q")); qs != "" {
		fq := `"` + strings.ReplaceAll(qs, `"`, `""`) + `"`
		res, err := s.st.Search(fq, f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, c := range res {
			j := s.toJSON(c)
			j.Snippet = s.snippetHTML(strings.TrimSpace(c.Snippet))
			out = append(out, j)
		}
	} else {
		res, err := s.st.List(f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, c := range res {
			out = append(out, s.toJSON(c))
		}
	}
	writeJSON(w, map[string]any{"commands": out})
}

func (s *webServer) command(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/cmd/"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	c, err := s.st.Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	j := s.toJSON(*c)
	var html string
	if s.redact {
		// redact on plain text so escape sequences can't split a secret;
		// rendered without colors by design
		plain := string(ansi.Strip(c.Output))
		red, _ := redact.String(plain, s.extra)
		html = htmlEscape(red)
	} else {
		html = ansihtml.Render(c.Output)
	}
	writeJSON(w, map[string]any{"meta": j, "html": html})
}

func (s *webServer) diff(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/diff/"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cur, err := s.st.Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	prev, err := s.st.PrevSame(cur.ID, cur.Cmd)
	if err != nil || prev == nil {
		writeJSON(w, map[string]any{"prev": nil})
		return
	}
	pfull, err := s.st.Get(prev.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	aTxt := string(ansi.Strip(pfull.Output))
	bTxt := string(ansi.Strip(cur.Output))
	if s.redact {
		aTxt, _ = redact.String(aTxt, s.extra)
		bTxt, _ = redact.String(bTxt, s.extra)
	}
	ops := diff.Lines(aTxt, bTxt)
	var sb strings.Builder
	same := true
	for _, op := range ops {
		switch op.Kind {
		case '-':
			same = false
			sb.WriteString(`<span class="dl">- ` + htmlEscape(op.Line) + "</span>\n")
		case '+':
			same = false
			sb.WriteString(`<span class="in">+ ` + htmlEscape(op.Line) + "</span>\n")
		default:
			sb.WriteString("  " + htmlEscape(op.Line) + "\n")
		}
	}
	writeJSON(w, map[string]any{
		"prev": s.toJSON(*pfull), "same": same, "html": sb.String(),
	})
}

func (s *webServer) stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"commands": st.Commands, "sessions": st.Sessions,
		"raw_bytes": st.RawBytes, "db_bytes": st.DBBytes,
		"redacting": s.redact, "version": version,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
