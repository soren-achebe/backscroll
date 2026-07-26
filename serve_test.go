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
