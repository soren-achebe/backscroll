package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
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
	doOpen := fs.Bool("open", false, "open the UI in your default browser once listening")
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
	mux.HandleFunc("/export/", srv.exportOne)

	fmt.Printf("backscroll web UI: \x1b[1mhttp://%s/\x1b[0m  (Ctrl-C to stop; read-only)\n", ln.Addr())
	if *doOpen {
		go openBrowser(browseURL(ln.Addr().String()))
	}
	s := &http.Server{Handler: srv.guard(mux), ReadHeaderTimeout: 5 * time.Second}
	return s.Serve(ln)
}

// browseURL turns the listener address into something a browser can
// open: wildcard binds (0.0.0.0 / [::]) aren't dialable, so point the
// browser at loopback on the same port.
func browseURL(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "http://" + hostport + "/"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// openBrowser best-effort opens url in the default browser; a miss is
// not an error worth failing the server over (the URL is printed).
func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	c.Stdout, c.Stderr = nil, nil
	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "--open: %v\n", err)
		return
	}
	go c.Wait() // reap; xdg-open returns quickly
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
	Hist      bool   `json:"hist,omitempty"`    // shell-history import (no output exists)
	Snippet   string `json:"snippet,omitempty"` // HTML with <mark>
	Ctx       string `json:"ctx,omitempty"`     // HTML context hunks (search + ctx=N)
}

func (s *webServer) toJSON(c store.Command) cmdJSON {
	j := cmdJSON{
		ID: c.ID, Cmd: c.Cmd, Cwd: c.Cwd, Bytes: c.OutputLen,
		Truncated: c.Truncated, Host: c.Host, Session: c.SessionID, DurMs: -1,
		Hist: strings.HasPrefix(c.Machine, "hist:"),
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

// filterFromQuery builds the shared store.Filter from URL query params
// (session/cwd/host/exit/since). Limit is left for the caller to set.
func filterFromQuery(q url.Values) store.Filter {
	var f store.Filter
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
		if t, err := parseTimeSpec(v); err == nil {
			f.Since = t
		}
	}
	if v := q.Get("until"); v != "" {
		if t, err := parseTimeSpec(v); err == nil {
			f.Until = t
		}
	}
	return f
}

func (s *webServer) commands(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := filterFromQuery(q)
	f.Limit = 50
	if n, err := strconv.Atoi(q.Get("n")); err == nil && n > 0 && n <= 500 {
		f.Limit = n
	}
	ctxN := 0
	if n, err := strconv.Atoi(q.Get("ctx")); err == nil && n > 0 {
		if n > 10 {
			n = 10
		}
		ctxN = n
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
			if ctxN > 0 {
				j.Ctx = s.ctxHTML(c, qs, ctxN)
			}
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

// ctxHTML renders grep-style context hunks from a search hit's stored
// plain output as HTML (search with ctx=N — parity with `search -C`).
// Redaction runs on the whole text BEFORE matching and highlighting, so
// highlight markup can never split a secret past the redact patterns.
func (s *webServer) ctxHTML(c store.Command, query string, n int) string {
	const maxHunks = 5
	text, err := s.st.Plain(c.ID)
	if err != nil || text == "" {
		return ""
	}
	if s.redact {
		text, _ = redact.String(text, s.extra)
	}
	hunks, shown, total := grepContext(text, query, n, n, maxHunks)
	if total == 0 {
		return ""
	}
	const so, se = "\x00S\x00", "\x00E\x00"
	var sb strings.Builder
	for hi, h := range hunks {
		if hi > 0 {
			sb.WriteString("<span class=\"cx\">     --</span>\n")
		}
		for i, ln := range h.Lines {
			no := h.Start + i + 1
			if h.IsMatch[i] {
				clipped := clipLine(ln, matchOffset(ln, query), 220)
				esc := htmlEscape(highlightMatches(clipped, query, so, se))
				esc = strings.ReplaceAll(esc, so, "<mark>")
				esc = strings.ReplaceAll(esc, se, "</mark>")
				fmt.Fprintf(&sb, "<span class=\"cn\">%5d:</span>%s\n", no, esc)
			} else {
				fmt.Fprintf(&sb, "<span class=\"cx\">%5d- %s</span>\n", no, htmlEscape(clipLine(ln, 0, 220)))
			}
		}
	}
	if total > shown {
		fmt.Fprintf(&sb, "<span class=\"cx\">(%d more matching lines)</span>\n", total-shown)
	}
	return sb.String()
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

// exportOne serves /export/<id>.html — the same self-contained page
// `export --format html` produces for that command, as a download.
// Under --redact the output is redacted on plain text before rendering
// (colors are lost, mirroring /api/cmd/): escape sequences inside raw
// output could otherwise split a secret past the redact patterns.
func (s *webServer) exportOne(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/export/")
	id, err := strconv.ParseInt(strings.TrimSuffix(name, ".html"), 10, 64)
	if err != nil || !strings.HasSuffix(name, ".html") {
		http.Error(w, "want /export/<id>.html", http.StatusBadRequest)
		return
	}
	c, err := s.st.Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if s.redact {
		c.Cmd, _ = redact.String(c.Cmd, s.extra)
		red, _ := redact.Bytes(ansi.Strip(c.Output), s.extra)
		c.Output = red
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"backscroll-%d.html\"", id))
	exportHTML(w, []*store.Command{c})
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
	q := r.URL.Query()
	if dim := q.Get("by"); dim != "" {
		s.breakdown(w, dim, q)
		return
	}
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

type statJSON struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
	Fails int    `json:"fails"`
	DurMs int64  `json:"dur_ms"`
	// Raw is the unformatted group key for dimensions that map onto a
	// /api/commands filter param (cwd, host, exit) — the UI uses it to
	// make breakdown rows clickable. Omitted when the key is not
	// filterable (cmd, day, "?" exits) or when --redact altered it
	// (a redacted string would filter to nothing).
	Raw string `json:"raw,omitempty"`
	// Spark is the per-time-bucket activity count across the filtered
	// range (sparkBuckets buckets, oldest first); omitted when no
	// command in the set has a start time.
	Spark []int `json:"spark,omitempty"`
}

// statRawKey returns the filterable raw key for a breakdown row, or ""
// when the row cannot be turned into a /api/commands filter.
func statRawKey(dim, key string, redacting bool, extra []*regexp.Regexp) string {
	switch dim {
	case "cwd", "dir", "host":
	case "exit":
		if _, err := strconv.ParseInt(key, 10, 64); err != nil {
			return "" // "?" — no exit recorded, not expressible as exit=N
		}
	case "day", "date":
		if _, err := time.ParseInLocation("2006-01-02", key, time.Local); err != nil {
			return "" // "unknown" — no start time recorded
		}
	default:
		return ""
	}
	if redacting {
		if red, _ := redact.String(key, extra); red != key {
			return "" // redaction changed it; the raw value must not leak
		}
	}
	return key
}

// breakdown serves /api/stats?by=cmd|cwd|exit|host|day scoped by the
// shared filters — the web-UI face of `backscroll stats --by`.
func (s *webServer) breakdown(w http.ResponseWriter, dim string, q url.Values) {
	switch dim {
	case "cmd", "cwd", "exit", "host", "day":
	default:
		http.Error(w, "by: want cmd, cwd, exit, host or day", http.StatusBadRequest)
		return
	}
	f := filterFromQuery(q)
	f.Limit = 1 << 30
	cmds, err := s.st.List(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	list := groupStats(cmds, dim)
	topN := 15
	if n, err := strconv.Atoi(q.Get("n")); err == nil && n > 0 && n <= 100 {
		topN = n
	}
	shown := list
	if len(list) > topN {
		if dim == "day" {
			shown = list[len(list)-topN:] // most recent days
		} else {
			shown = list[:topN]
		}
	}
	home, _ := os.UserHomeDir()
	out := make([]statJSON, 0, len(shown))
	for _, g := range shown {
		key := statDisplayKey(dim, g.key, home)
		if s.redact {
			key, _ = redact.String(key, s.extra)
		}
		j := statJSON{
			Key: key, Count: g.count, Fails: g.fails,
			DurMs: g.dur.Milliseconds(),
			Raw:   statRawKey(dim, g.key, s.redact, s.extra),
		}
		if dim != "day" { // day rows already are a time histogram
			j.Spark = g.spark
		}
		out = append(out, j)
	}
	resp := map[string]any{
		"by": dim, "groups": out, "total": len(cmds), "distinct": len(list),
	}
	if first, last, ok := statTimeRange(cmds); ok && dim != "day" {
		resp["spark_from"] = first.Local().Format("2006-01-02")
		resp["spark_to"] = last.Local().Format("2006-01-02")
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
