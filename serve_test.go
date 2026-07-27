package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/store"
)

func newTestServer(t *testing.T, redactOn bool) (*webServer, http.Handler) {
	t.Helper()
	t.Setenv("BACKSCROLL_DB", filepath.Join(t.TempDir(), "test.db"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	sess, err := st.NewSession("bash", "xterm")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	add := func(cmd string, exit int, out string) {
		t.Helper()
		raw := []byte(out)
		if err := st.AddCommand(sess, cmd, "/tmp", exit, true, t0, t0.Add(time.Second),
			raw, false, string(ansi.Strip(raw))); err != nil {
			t.Fatal(err)
		}
		t0 = t0.Add(time.Minute)
	}
	add("curl -s https://api.example.com/health", 0, "status: \x1b[32mok\x1b[0m\nlatency: 12ms\n")
	add("grep -r TODO .", 1, "")
	add("curl -s https://api.example.com/health", 0, "status: \x1b[31mDEGRADED\x1b[0m\nlatency: 900ms\n")
	add("echo secret", 0, "AWS_SECRET_ACCESS_KEY=AKIAIOSFODNN7EXAMPLE\n<script>alert(1)</script>\n")

	srv := &webServer{st: st, redact: redactOn, checkHost: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.index)
	mux.HandleFunc("/api/commands", srv.commands)
	mux.HandleFunc("/api/cmd/", srv.command)
	mux.HandleFunc("/api/diff/", srv.diff)
	mux.HandleFunc("/api/stats", srv.stats)
	mux.HandleFunc("/export/", srv.exportOne)
	return srv, srv.guard(mux)
}

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://127.0.0.1:4133"+path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body map[string]any
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad json from %s: %v", path, err)
		}
	}
	return w, body
}

func TestServeIndex(t *testing.T) {
	_, h := newTestServer(t, false)
	w, _ := get(t, h, "/")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "backscroll") {
		t.Fatalf("code=%d", w.Code)
	}
	// non-root path under / must 404, not serve the page
	w, _ = get(t, h, "/nonsense")
	if w.Code != 404 {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestServeList(t *testing.T) {
	_, h := newTestServer(t, false)
	w, body := get(t, h, "/api/commands")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	cs := body["commands"].([]any)
	if len(cs) != 4 {
		t.Fatalf("want 4 commands, got %d", len(cs))
	}
	// newest first
	first := cs[0].(map[string]any)
	if first["cmd"] != "echo secret" {
		t.Fatalf("order: %v", first["cmd"])
	}
}

func TestServeListFilters(t *testing.T) {
	_, h := newTestServer(t, false)
	_, body := get(t, h, "/api/commands?exit=fail")
	cs := body["commands"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["cmd"] != "grep -r TODO ." {
		t.Fatalf("exit=fail: %v", cs)
	}
}

func TestServeSearch(t *testing.T) {
	_, h := newTestServer(t, false)
	_, body := get(t, h, "/api/commands?q=latency")
	cs := body["commands"].([]any)
	if len(cs) != 2 {
		t.Fatalf("want 2 matches, got %d", len(cs))
	}
	snip := cs[0].(map[string]any)["snippet"].(string)
	// trigram tokenizer may highlight a partial token — just require a mark
	if !strings.Contains(snip, "<mark>laten") {
		t.Fatalf("snippet: %q", snip)
	}
	if strings.Contains(snip, "\x1b") {
		t.Fatalf("raw escape leaked into snippet: %q", snip)
	}
}

func TestServeGetRendersANSI(t *testing.T) {
	_, h := newTestServer(t, false)
	_, body := get(t, h, "/api/commands?q=DEGRADED")
	id := int64(body["commands"].([]any)[0].(map[string]any)["id"].(float64))
	_, body = get(t, h, "/api/cmd/"+itoa(id))
	html := body["html"].(string)
	if !strings.Contains(html, `<span class="f1">DEGRADED</span>`) {
		t.Fatalf("html: %q", html)
	}
}

func TestServeXSSEscaped(t *testing.T) {
	_, h := newTestServer(t, false)
	_, body := get(t, h, "/api/cmd/4")
	html := body["html"].(string)
	if strings.Contains(html, "<script>") {
		t.Fatalf("unescaped script tag: %q", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("expected escaped tag: %q", html)
	}
}

func TestServeRedact(t *testing.T) {
	_, h := newTestServer(t, true)
	_, body := get(t, h, "/api/cmd/4")
	html := body["html"].(string)
	if strings.Contains(html, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secret leaked: %q", html)
	}
	_, body = get(t, h, "/api/stats")
	if body["redacting"] != true {
		t.Fatal("stats should report redacting")
	}
}

func TestServeDiff(t *testing.T) {
	_, h := newTestServer(t, false)
	// id 3 is the second health check; prev same = id 1
	_, body := get(t, h, "/api/diff/3")
	prev := body["prev"].(map[string]any)
	if int64(prev["id"].(float64)) != 1 {
		t.Fatalf("prev: %v", prev)
	}
	if body["same"] == true {
		t.Fatal("outputs differ")
	}
	html := body["html"].(string)
	if !strings.Contains(html, `<span class="dl">- status: ok</span>`) ||
		!strings.Contains(html, `<span class="in">+ status: DEGRADED</span>`) {
		t.Fatalf("diff html: %q", html)
	}
	// no previous run
	_, body = get(t, h, "/api/diff/2")
	if body["prev"] != nil {
		t.Fatalf("want null prev, got %v", body["prev"])
	}
}

func TestServeGuardHost(t *testing.T) {
	_, h := newTestServer(t, false)
	for _, host := range []string{"localhost:4133", "127.0.0.1:4133", "[::1]:4133", "localhost"} {
		req := httptest.NewRequest("GET", "/api/stats", nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("host %q: %d", host, w.Code)
		}
	}
	for _, host := range []string{"evil.example.com", "evil.example.com:4133", "192.168.1.5:4133"} {
		req := httptest.NewRequest("GET", "/api/stats", nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("host %q: want 403, got %d", host, w.Code)
		}
	}
}

func TestServeReadOnly(t *testing.T) {
	_, h := newTestServer(t, false)
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		req := httptest.NewRequest(m, "http://127.0.0.1:4133/api/commands", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: want 405, got %d", m, w.Code)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestServeSearchContext(t *testing.T) {
	_, h := newTestServer(t, false)
	_, body := get(t, h, "/api/commands?q=latency&ctx=1")
	cs := body["commands"].([]any)
	if len(cs) != 2 {
		t.Fatalf("want 2 matches, got %d", len(cs))
	}
	ctx := cs[0].(map[string]any)["ctx"].(string)
	if !strings.Contains(ctx, "<mark>latency</mark>") {
		t.Fatalf("no highlighted match in ctx: %q", ctx)
	}
	// the line before ("status: …") must appear as dim context with its number
	if !strings.Contains(ctx, `<span class="cx">    1- status:`) {
		t.Fatalf("missing context line: %q", ctx)
	}
	if !strings.Contains(ctx, `<span class="cn">    2:</span>`) {
		t.Fatalf("missing match line number: %q", ctx)
	}
	if strings.Contains(ctx, "\x1b") || strings.Contains(ctx, "\x00") {
		t.Fatalf("raw escape/sentinel leaked: %q", ctx)
	}
	// without ctx param there is no ctx field
	_, body = get(t, h, "/api/commands?q=latency")
	if _, ok := cs[0].(map[string]any)["snippet"]; !ok {
		t.Fatal("snippet should still be present")
	}
	if c := body["commands"].([]any)[0].(map[string]any)["ctx"]; c != nil {
		t.Fatalf("ctx should be omitted, got %v", c)
	}
}

func TestServeSearchContextRedact(t *testing.T) {
	_, h := newTestServer(t, true)
	// redaction runs before matching: a query hitting only the secret's
	// masked span yields no context (the secret must not leak)
	_, body := get(t, h, "/api/commands?q=AKIAIOSFODNN7EXAMPLE&ctx=2")
	for _, c := range body["commands"].([]any) {
		if ctx, _ := c.(map[string]any)["ctx"].(string); strings.Contains(ctx, "AKIAIOSFODNN7EXAMPLE") {
			t.Fatalf("secret leaked in ctx: %q", ctx)
		}
	}
	// a non-secret query on the same row still gets context, redacted
	_, body = get(t, h, "/api/commands?q=alert&ctx=2")
	cs := body["commands"].([]any)
	if len(cs) != 1 {
		t.Fatalf("want 1 match, got %d", len(cs))
	}
	ctx := cs[0].(map[string]any)["ctx"].(string)
	if !strings.Contains(ctx, "<mark>alert</mark>") {
		t.Fatalf("no match in ctx: %q", ctx)
	}
	if strings.Contains(ctx, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secret leaked in context lines: %q", ctx)
	}
}

func TestServeStatsBreakdown(t *testing.T) {
	_, h := newTestServer(t, false)
	_, body := get(t, h, "/api/stats?by=cmd")
	gs := body["groups"].([]any)
	if len(gs) != 3 {
		t.Fatalf("want 3 groups, got %v", gs)
	}
	top := gs[0].(map[string]any)
	if top["key"] != "curl" || top["count"].(float64) != 2 {
		t.Fatalf("top group: %v", top)
	}
	if body["total"].(float64) != 4 || body["distinct"].(float64) != 3 {
		t.Fatalf("totals: %v %v", body["total"], body["distinct"])
	}

	// filters scope the breakdown
	_, body = get(t, h, "/api/stats?by=cmd&exit=fail")
	gs = body["groups"].([]any)
	if len(gs) != 1 || gs[0].(map[string]any)["key"] != "grep" {
		t.Fatalf("filtered groups: %v", gs)
	}

	// exit dimension gets display keys
	_, body = get(t, h, "/api/stats?by=exit")
	keys := map[string]bool{}
	for _, g := range body["groups"].([]any) {
		keys[g.(map[string]any)["key"].(string)] = true
	}
	if !keys["0"] || !keys["1"] {
		t.Fatalf("exit keys: %v", keys)
	}

	// day dimension: single chronological bucket for our seed data
	_, body = get(t, h, "/api/stats?by=day")
	if gs := body["groups"].([]any); len(gs) != 1 || gs[0].(map[string]any)["count"].(float64) != 4 {
		t.Fatalf("day groups: %v", gs)
	}

	// bad dimension → 400
	if w, _ := get(t, h, "/api/stats?by=bogus"); w.Code != http.StatusBadRequest {
		t.Fatalf("bogus dim: want 400, got %d", w.Code)
	}
}

func TestServeStatsRawKeys(t *testing.T) {
	_, h := newTestServer(t, false)

	// cmd dim is not filterable — no raw key on any row
	_, cmdBody := get(t, h, "/api/stats?by=cmd")
	for _, g := range cmdBody["groups"].([]any) {
		if _, ok := g.(map[string]any)["raw"]; ok {
			t.Fatalf("by=cmd row unexpectedly has raw: %v", g)
		}
	}

	// day rows carry the date as raw so the UI can build a since/until window
	_, dayBody := get(t, h, "/api/stats?by=day")
	dayGroups := dayBody["groups"].([]any)
	dayRaw, _ := dayGroups[0].(map[string]any)["raw"].(string)
	if _, err := time.Parse("2006-01-02", dayRaw); err != nil {
		t.Fatalf("day raw key: got %q, want a date", dayRaw)
	}

	// exit/cwd/host rows carry the raw key that /api/commands accepts
	_, body := get(t, h, "/api/stats?by=exit")
	raws := map[string]bool{}
	for _, g := range body["groups"].([]any) {
		if r, ok := g.(map[string]any)["raw"].(string); ok {
			raws[r] = true
		}
	}
	if !raws["0"] || !raws["1"] {
		t.Fatalf("exit raw keys: %v", raws)
	}
	_, body = get(t, h, "/api/stats?by=cwd")
	if g := body["groups"].([]any)[0].(map[string]any); g["raw"] != "/tmp" {
		t.Fatalf("cwd raw: %v", g)
	}
	_, body = get(t, h, "/api/stats?by=host")
	if g := body["groups"].([]any)[0].(map[string]any); g["raw"] != "local" {
		t.Fatalf("host raw: %v", g)
	}

	// round trip: the raw key really filters /api/commands
	_, body = get(t, h, "/api/commands?exit=1")
	if cs := body["commands"].([]any); len(cs) != 1 {
		t.Fatalf("exit=1 round trip: %v", cs)
	}

	// day round trip: since=<day>&until=<day+1> selects everything (all
	// seed rows are recent), and an old window selects nothing
	_, dayBody = get(t, h, "/api/stats?by=day")
	day := dayBody["groups"].([]any)[0].(map[string]any)["raw"].(string)
	next := time.Now().AddDate(0, 0, 2).Format("2006-01-02")
	_, body = get(t, h, "/api/commands?since="+day+"&until="+next)
	all := len(body["commands"].([]any))
	if all == 0 {
		t.Fatalf("day window round trip: no commands")
	}
	_, body = get(t, h, "/api/commands?since=2001-01-01&until=2001-01-02")
	if cs, ok := body["commands"].([]any); ok && len(cs) != 0 {
		t.Fatalf("old window: want none, got %v", cs)
	}
}

func TestStatRawKey(t *testing.T) {
	if got := statRawKey("exit", "?", false, nil); got != "" {
		t.Fatalf("'?' exit must not be filterable, got %q", got)
	}
	if got := statRawKey("cmd", "git", false, nil); got != "" {
		t.Fatalf("cmd dim must not be filterable, got %q", got)
	}
	if got := statRawKey("day", "2026-07-26", false, nil); got != "2026-07-26" {
		t.Fatalf("day date should be filterable, got %q", got)
	}
	if got := statRawKey("day", "unknown", false, nil); got != "" {
		t.Fatalf("'unknown' day must not be filterable, got %q", got)
	}
	if got := statRawKey("cwd", "/tmp", true, nil); got != "/tmp" {
		t.Fatalf("unredacted cwd should pass through, got %q", got)
	}
	// a key the redactor rewrites must NOT leak via raw
	secret := "/home/x/AKIAIOSFODNN7EXAMPLE"
	if got := statRawKey("cwd", secret, true, nil); got != "" {
		t.Fatalf("redacted key leaked as raw: %q", got)
	}
}

func TestServeExportHTML(t *testing.T) {
	srv, h := newTestServer(t, false)

	w, _ := get(t, h, "/export/1.html")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="backscroll-1.html"` {
		t.Fatalf("content-disposition %q", cd)
	}
	page := w.Body.String()
	if !strings.Contains(page, "<!doctype html>") ||
		!strings.Contains(page, "curl -s https://api.example.com/health") {
		t.Fatalf("page missing pieces:\n%s", page)
	}
	if strings.Contains(page, "\x1b[") {
		t.Fatal("raw ANSI leaked into export page")
	}
	// parity: byte-identical to what `export --format html` writes
	c, err := srv.st.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := exportHTML(&buf, []*store.Command{c}); err != nil {
		t.Fatal(err)
	}
	if page != buf.String() {
		t.Fatal("serve download differs from export --format html")
	}

	for path, want := range map[string]int{
		"/export/999.html": 404,
		"/export/1":        400,
		"/export/x.html":   400,
		"/export/":         400,
	} {
		if w, _ := get(t, h, path); w.Code != want {
			t.Fatalf("%s: status %d, want %d", path, w.Code, want)
		}
	}
}

func TestServeExportHTMLRedact(t *testing.T) {
	_, h := newTestServer(t, true)
	w, _ := get(t, h, "/export/4.html")
	page := w.Body.String()
	if strings.Contains(page, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secret leaked:\n%s", page)
	}
	if !strings.Contains(page, "[REDACTED]") {
		t.Fatal("expected [REDACTED] marker")
	}
	if strings.Contains(page, "<script>") {
		t.Fatal("unescaped script tag")
	}
	// redaction runs on plain text: rendered output must carry no color spans
	out := page[strings.Index(page, "<pre>"):]
	if strings.Contains(out, `class="f`) {
		t.Fatal("redacted export should be colorless")
	}
}

func TestBrowseURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:4133": "http://127.0.0.1:4133/",
		"0.0.0.0:4133":   "http://127.0.0.1:4133/",
		"[::]:8080":      "http://127.0.0.1:8080/",
		"[::1]:4133":     "http://[::1]:4133/",
		"localhost:99":   "http://localhost:99/",
	}
	for in, want := range cases {
		if got := browseURL(in); got != want {
			t.Errorf("browseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServeStatsSpark(t *testing.T) {
	_, h := newTestServer(t, false)
	_, body := get(t, h, "/api/stats?by=cmd")
	if body["spark_from"] == nil || body["spark_to"] == nil {
		t.Fatalf("missing spark range: %v", body)
	}
	top := body["groups"].([]any)[0].(map[string]any) // curl, 2 runs
	sp, ok := top["spark"].([]any)
	if !ok || len(sp) != sparkBuckets {
		t.Fatalf("spark = %v, want %d buckets", top["spark"], sparkBuckets)
	}
	sum := 0.0
	for _, v := range sp {
		sum += v.(float64)
	}
	if sum != top["count"].(float64) {
		t.Errorf("spark sum %v != count %v", sum, top["count"])
	}

	// day rows are already a time histogram — no spark, no range
	_, body = get(t, h, "/api/stats?by=day")
	if body["spark_from"] != nil {
		t.Errorf("day breakdown has spark range: %v", body["spark_from"])
	}
	if g := body["groups"].([]any)[0].(map[string]any); g["spark"] != nil {
		t.Errorf("day group has spark: %v", g["spark"])
	}
}
