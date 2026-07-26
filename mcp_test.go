package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/soren-achebe/backscroll/internal/redact"
	"github.com/soren-achebe/backscroll/internal/store"
)

// mcpHarness drives an mcpServer over in-memory pipes, one JSON-RPC
// exchange at a time.
type mcpHarness struct {
	t    *testing.T
	in   io.WriteCloser
	out  *bufio.Scanner
	done chan error
}

func newMCPHarness(t *testing.T, s *mcpServer) *mcpHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &mcpHarness{t: t, in: inW, done: make(chan error, 1)}
	h.out = bufio.NewScanner(outR)
	h.out.Buffer(make([]byte, 64<<10), 16<<20)
	go func() { h.done <- s.serve(inR, outW); outW.Close() }()
	t.Cleanup(func() {
		inW.Close()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("serve did not exit")
		}
	})
	return h
}

// call sends a request and returns the raw result; fails the test on a
// JSON-RPC-level error.
func (h *mcpHarness) call(method string, params any) json.RawMessage {
	h.t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := h.in.Write(append(b, '\n')); err != nil {
		h.t.Fatalf("write: %v", err)
	}
	if !h.out.Scan() {
		h.t.Fatalf("no response to %s (scan err: %v)", method, h.out.Err())
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &resp); err != nil {
		h.t.Fatalf("bad response %q: %v", h.out.Text(), err)
	}
	if resp.Error != nil {
		h.t.Fatalf("%s: rpc error %d %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result
}

// notify sends a notification (no id) and expects no response.
func (h *mcpHarness) notify(method string) {
	h.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if _, err := h.in.Write(append(b, '\n')); err != nil {
		h.t.Fatalf("write: %v", err)
	}
}

// toolText calls tools/call and returns (text, isError).
func (h *mcpHarness) toolText(name string, args any) (string, bool) {
	h.t.Helper()
	res := h.call("tools/call", map[string]any{"name": name, "arguments": args})
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		h.t.Fatalf("bad tool result %s: %v", res, err)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		h.t.Fatalf("want one text content block, got %s", res)
	}
	return r.Content[0].Text, r.IsError
}

func mcpTestStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("BACKSCROLL_DB", filepath.Join(t.TempDir(), "test.db"))
	st, err := store.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess, err := st.NewSession("/bin/bash", "xterm")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	base := time.Now().Add(-time.Hour)
	add := func(cmd, cwd string, exit int, out string) {
		t.Helper()
		at := base
		base = base.Add(time.Minute)
		if err := st.AddCommand(sess, cmd, cwd, exit, true, at, at.Add(2*time.Second),
			[]byte(out), false, out); err != nil {
			t.Fatalf("AddCommand(%q): %v", cmd, err)
		}
	}
	add("make test", "/home/u/proj", 0, "ok  \tproj\t0.31s\nPASS\n")
	add("curl api.internal/health", "/home/u/proj", 7, "curl: (7) connection refused\n")
	add("make test", "/home/u/proj", 1, "FAIL\tproj\t0.29s\n--- FAIL: TestX\nPASS count dropped\n")
	add("echo secret", "/tmp", 0, "token=ghp_abcdefghijklmnopqrstuvwxyz0123456789\n")
	return st
}

func TestMCPHandshakeAndToolsList(t *testing.T) {
	h := newMCPHarness(t, &mcpServer{st: mcpTestStore(t)})
	res := h.call("initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		t.Fatalf("bad initialize result: %v", err)
	}
	if init.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocolVersion: want echo of client's, got %q", init.ProtocolVersion)
	}
	if init.ServerInfo.Name != "backscroll" {
		t.Errorf("serverInfo.name = %q", init.ServerInfo.Name)
	}
	if _, ok := init.Capabilities["tools"]; !ok {
		t.Error("capabilities.tools missing")
	}
	h.notify("notifications/initialized")

	res = h.call("tools/list", nil)
	var tl struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &tl); err != nil {
		t.Fatalf("bad tools/list result: %v", err)
	}
	want := []string{"search_output", "list_commands", "get_output", "diff_output"}
	if len(tl.Tools) != len(want) {
		t.Fatalf("want %d tools, got %d", len(want), len(tl.Tools))
	}
	for i, tool := range tl.Tools {
		if tool.Name != want[i] {
			t.Errorf("tool %d = %q, want %q", i, tool.Name, want[i])
		}
		if tool.Description == "" || len(tool.InputSchema) == 0 {
			t.Errorf("tool %q missing description or schema", tool.Name)
		}
	}
	// ping and unknown method
	h.call("ping", nil)
}

func TestMCPUnknownMethodAndParseError(t *testing.T) {
	h := newMCPHarness(t, &mcpServer{st: mcpTestStore(t)})
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "nope"})
	h.in.Write(append(b, '\n'))
	h.out.Scan()
	var resp struct {
		ID    int       `json:"id"`
		Error *rpcError `json:"error"`
	}
	json.Unmarshal(h.out.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != -32601 || resp.ID != 9 {
		t.Errorf("want -32601 with id echoed, got %s", h.out.Text())
	}
	h.in.Write([]byte("{not json\n"))
	h.out.Scan()
	var resp2 struct {
		Error *rpcError `json:"error"`
	}
	json.Unmarshal(h.out.Bytes(), &resp2)
	if resp2.Error == nil || resp2.Error.Code != -32700 {
		t.Errorf("want -32700 parse error, got %s", h.out.Text())
	}
}

func TestMCPSearchListGet(t *testing.T) {
	h := newMCPHarness(t, &mcpServer{st: mcpTestStore(t)})

	text, isErr := h.toolText("search_output", map[string]any{"query": "connection refused"})
	if isErr || !strings.Contains(text, "curl api.internal/health") || !strings.Contains(text, "exit 7") {
		t.Errorf("search: %v %q", isErr, text)
	}

	text, isErr = h.toolText("list_commands", map[string]any{"exit": "fail"})
	if isErr || !strings.Contains(text, "2 command(s)") {
		t.Errorf("list fail: %v %q", isErr, text)
	}
	if strings.Contains(text, "echo secret") {
		t.Errorf("list fail should exclude exit-0 commands: %q", text)
	}

	// id -1 = most recent; its output holds a token that must be redacted
	// when redaction is on (separate test) but visible here (redact off).
	text, isErr = h.toolText("get_output", map[string]any{"id": -1})
	if isErr || !strings.Contains(text, "$ echo secret") || !strings.Contains(text, "ghp_abcdefghijklmnop") {
		t.Errorf("get -1: %v %q", isErr, text)
	}

	// missing id → tool error, not a crash
	_, isErr = h.toolText("get_output", map[string]any{})
	if !isErr {
		t.Error("get without id should be a tool error")
	}
	_, isErr = h.toolText("get_output", map[string]any{"id": 99999})
	if !isErr {
		t.Error("get of missing id should be a tool error")
	}
	_, isErr = h.toolText("search_output", map[string]any{"query": "  "})
	if !isErr {
		t.Error("blank query should be a tool error")
	}
	_, isErr = h.toolText("nope_tool", map[string]any{})
	if !isErr {
		t.Error("unknown tool should be a tool error")
	}
}

func TestMCPDiffPrevSame(t *testing.T) {
	h := newMCPHarness(t, &mcpServer{st: mcpTestStore(t)})
	// entry 3 is the failing "make test"; diff vs previous run of same cmd
	text, isErr := h.toolText("diff_output", map[string]any{"id": 3})
	if isErr {
		t.Fatalf("diff: %q", text)
	}
	for _, want := range []string{"--- #1", "+++ #3", "-PASS", "+--- FAIL: TestX"} {
		if !strings.Contains(text, want) {
			t.Errorf("diff missing %q in %q", want, text)
		}
	}
	// no previous run → tool error
	_, isErr = h.toolText("diff_output", map[string]any{"id": 2})
	if !isErr {
		t.Error("diff with no previous run should be a tool error")
	}
	// identical explicit pair
	text, _ = h.toolText("diff_output", map[string]any{"id": 1, "other": 1})
	if !strings.Contains(text, "identical") {
		t.Errorf("self-diff: %q", text)
	}
}

func TestMCPRedactDefault(t *testing.T) {
	s := &mcpServer{st: mcpTestStore(t), redact: true, extra: redact.LoadExtra()}
	h := newMCPHarness(t, s)
	text, isErr := h.toolText("get_output", map[string]any{"id": -1})
	if isErr {
		t.Fatalf("get: %q", text)
	}
	if strings.Contains(text, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("token leaked through redaction: %q", text)
	}
	if !strings.Contains(text, "token=") {
		t.Errorf("non-secret context should survive: %q", text)
	}
	// search snippets must be redacted too
	text, _ = h.toolText("search_output", map[string]any{"query": "token="})
	if strings.Contains(text, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("token leaked via search snippet: %q", text)
	}
}

func TestMCPMaxBytesTruncation(t *testing.T) {
	t.Setenv("BACKSCROLL_DB", filepath.Join(t.TempDir(), "test.db"))
	st, err := store.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	sess, _ := st.NewSession("/bin/bash", "xterm")
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "line %04d\n", i)
	}
	now := time.Now()
	if err := st.AddCommand(sess, "seq", "/tmp", 0, true, now, now.Add(time.Second),
		[]byte(b.String()), false, b.String()); err != nil {
		t.Fatal(err)
	}
	h := newMCPHarness(t, &mcpServer{st: st})
	text, isErr := h.toolText("get_output", map[string]any{"id": -1, "max_bytes": 2000})
	if isErr {
		t.Fatalf("get: %q", text)
	}
	if len(text) > 4000 {
		t.Errorf("truncated response too big: %d bytes", len(text))
	}
	for _, want := range []string{"line 0000", "line 4999", "bytes omitted"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in truncated output", want)
		}
	}
}
