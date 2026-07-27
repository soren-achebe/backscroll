package main

// backscroll mcp — a Model Context Protocol server over stdio, so AI
// coding agents can query your terminal history: "what did that command
// print?", "find the run where the build broke", "what changed in the
// output since it last passed?".
//
// Protocol: JSON-RPC 2.0, newline-delimited, stdin/stdout (the MCP
// "stdio" transport). No dependencies. Logs go to stderr only — stdout
// is the protocol channel.
//
// Privacy: outputs pass through secret redaction (built-in token
// patterns + ~/.config/backscroll/redact) by DEFAULT before being
// handed to a model. Opt out with --no-redact.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/soren-achebe/backscroll/internal/ansi"
	"github.com/soren-achebe/backscroll/internal/diff"
	"github.com/soren-achebe/backscroll/internal/redact"
	"github.com/soren-achebe/backscroll/internal/store"
)

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	noRedact := fs.Bool("no-redact", false, "do NOT mask secrets in output handed to the client")
	fs.Parse(args)
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	s := &mcpServer{st: st, redact: !*noRedact}
	if s.redact {
		s.extra = redact.LoadExtra()
	}
	return s.serve(os.Stdin, os.Stdout)
}

type mcpServer struct {
	st     *store.Store
	redact bool
	extra  []*regexp.Regexp
}

// ---- JSON-RPC plumbing ----

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *mcpServer) serve(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	w := bufio.NewWriter(out)
	reply := func(id json.RawMessage, result any, rerr *rpcError) {
		b, err := json.Marshal(rpcResp{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
		if err != nil { // result must marshal; fall back to an error reply
			b, _ = json.Marshal(rpcResp{JSONRPC: "2.0", ID: id,
				Error: &rpcError{Code: -32603, Message: "internal marshal error"}})
		}
		w.Write(b)
		w.WriteByte('\n')
		w.Flush()
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal(line, &req); err != nil {
			reply(json.RawMessage("null"), nil, &rpcError{Code: -32700, Message: "parse error"})
			continue
		}
		isNotification := len(req.ID) == 0 || string(req.ID) == "null"
		result, rerr := s.dispatch(req)
		if isNotification {
			continue // notifications never get a response
		}
		reply(req.ID, result, rerr)
	}
	return sc.Err()
}

func (s *mcpServer) dispatch(req rpcReq) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params), nil
	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed":
		return nil, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		return s.toolsCall(req.Params), nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

// mcpProtocolVersions we can speak, newest first.
var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

func (s *mcpServer) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(params, &p)
	ver := mcpProtocolVersions[0]
	for _, v := range mcpProtocolVersions {
		if p.ProtocolVersion == v {
			ver = v
			break
		}
	}
	return map[string]any{
		"protocolVersion": ver,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "backscroll", "version": version},
		"instructions": "backscroll records every terminal command the user runs with its FULL " +
			"output, exit code, working directory and timing (locally, in SQLite). " +
			"Use search_output to find commands whose output or command line matched a string " +
			"(e.g. an error message), get_output to read exactly what a command printed " +
			"(id -1 = the user's most recent command), list_commands for recent history " +
			"(e.g. only failures), and diff_output to see how a command's output changed " +
			"versus its previous run. Prefer these tools over re-running expensive or " +
			"side-effectful commands: the answer to \"what did that print?\" is usually " +
			"already recorded.",
	}
}

// ---- tools ----

// roAnnotations marks a tool as a pure read of the local database: it never
// executes commands, never modifies history, and touches nothing outside the
// recorder's SQLite file.
func roAnnotations(title string) map[string]any {
	return map[string]any{
		"title":           title,
		"readOnlyHint":    true,
		"destructiveHint": false,
		"idempotentHint":  true,
		"openWorldHint":   false,
	}
}

var mcpTools = []map[string]any{
	{
		"name":        "search_output",
		"title":       "Search command outputs",
		"annotations": roAnnotations("Search command outputs"),
		"description": "Full-text search over recorded terminal commands AND their outputs — " +
			"finds e.g. every command that ever printed 'connection refused'. " +
			"Returns plain text, one block per match: id, time, cwd, exit code, the command " +
			"line, and a snippet around the match (secrets are masked by default). " +
			"Use this to FIND which command said something; use list_commands to browse " +
			"recent history without a search term, and get_output to read a full output " +
			"once you have an id. No matches returns an empty result, not an error. " +
			"Read-only: never re-runs anything.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string",
					"description": "text to search for in command lines and outputs (substring match; case-insensitive)"},
				"cwd": map[string]any{"type": "string",
					"description": "only commands run in this directory or beneath it (absolute path, or '.' for the server's cwd)"},
				"exit": map[string]any{"type": "string",
					"description": "only this exit code (a number), or 'fail' for any nonzero"},
				"since": map[string]any{"type": "string",
					"description": "only commands newer than this: 30m, 2h, 3d, 1w, or 2006-01-02[ 15:04]"},
				"until": map[string]any{"type": "string",
					"description": "only commands older than this (exclusive; same forms as since). Combine with since to bound a time window, e.g. one day."},
				"host": map[string]any{"type": "string",
					"description": "only commands from this synced machine ('local' = this machine)"},
				"limit": map[string]any{"type": "integer",
					"description": "max results (default 20, max 100)"},
				"context_lines": map[string]any{"type": "integer",
					"description": "instead of a short snippet, show every matching output line " +
						"with this many lines of context before and after (grep -C style, " +
						"with line numbers; 0 = matching lines only, max 10). Often avoids " +
						"a follow-up get_output call."},
			},
			"required": []string{"query"},
		},
	},
	{
		"name":        "list_commands",
		"title":       "List recent commands",
		"annotations": roAnnotations("List recent commands"),
		"description": "List the user's recent terminal commands (most recent first) as plain " +
			"text: one entry per command with id, time, cwd, exit code, duration and output " +
			"size — command lines only, no output text. Supports the same filters as " +
			"search_output, e.g. exit='fail' for recent failures or cwd='.' for this project " +
			"only. Use this to browse history; use search_output when looking for specific " +
			"text, and get_output to read what a command actually printed. " +
			"Read-only: never re-runs anything.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cwd":   map[string]any{"type": "string", "description": "only commands run in this directory or beneath it"},
				"exit":  map[string]any{"type": "string", "description": "only this exit code (a number), or 'fail' for any nonzero"},
				"since": map[string]any{"type": "string", "description": "only commands newer than this: 30m, 2h, 3d, 1w, or a date"},
				"until": map[string]any{"type": "string", "description": "only commands older than this (exclusive; same forms as since)"},
				"host":  map[string]any{"type": "string", "description": "only commands from this synced machine ('local' = this machine)"},
				"limit": map[string]any{"type": "integer", "description": "max results (default 20, max 100)"},
			},
		},
	},
	{
		"name":        "get_output",
		"title":       "Get a command's output",
		"annotations": roAnnotations("Get a command's output"),
		"description": "Get the full recorded output of one terminal command, plus its command " +
			"line, exit code, cwd and timing, as plain text (ANSI escapes stripped; secrets " +
			"masked by default). id: a command id from search_output/list_commands, or " +
			"negative for relative addressing (-1 = the user's most recent command, -2 = the " +
			"one before it). Outputs larger than max_bytes return the head and tail around a " +
			"'[... N bytes omitted ...]' marker — call again with a larger max_bytes for more. " +
			"Errors with 'not found' if the id doesn't exist. " +
			"Read-only: reads the recording, never re-runs the command.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer",
					"description": "command id, or -N for Nth most recent (-1 = last command)"},
				"max_bytes": map[string]any{"type": "integer",
					"description": "cap on returned output size (default 51200); if the output is larger, the head and tail are returned with a gap marker"},
			},
			"required": []string{"id"},
		},
	},
	{
		"name":        "diff_output",
		"title":       "Diff two runs' outputs",
		"annotations": roAnnotations("Diff two runs' outputs"),
		"description": "Unified diff (diff -u style plain text) between the stored outputs of " +
			"two runs. With only 'id', diffs that command against the most recent EARLIER run " +
			"of the exact same command line — 'what changed since it last ran?'; errors if no " +
			"earlier identical command line exists. With 'other', diffs the two given commands " +
			"(other = older side). Identical outputs return a note saying so. Secrets are " +
			"masked by default. Read-only: diffs recordings, never re-runs anything.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer",
					"description": "newer command id (or -N for Nth most recent; -1 = last command)"},
				"other": map[string]any{"type": "integer",
					"description": "older command id to compare against (optional; default: previous run of the same command)"},
				"context": map[string]any{"type": "integer",
					"description": "lines of context around changes (default 3)"},
			},
			"required": []string{"id"},
		},
	},
}

func textResult(text string) any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func errResult(format string, a ...any) any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": fmt.Sprintf(format, a...)}},
		"isError": true,
	}
}

func (s *mcpServer) toolsCall(params json.RawMessage) any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errResult("bad tools/call params: %v", err)
	}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	switch p.Name {
	case "search_output":
		return s.toolSearch(args)
	case "list_commands":
		return s.toolList(args)
	case "get_output":
		return s.toolGet(args)
	case "diff_output":
		return s.toolDiff(args)
	default:
		return errResult("unknown tool %q", p.Name)
	}
}

type mcpFilterArgs struct {
	Cwd   string `json:"cwd"`
	Exit  string `json:"exit"`
	Since string `json:"since"`
	Until string `json:"until"`
	Host  string `json:"host"`
	Limit int    `json:"limit"`
}

func (a mcpFilterArgs) filter() (store.Filter, error) {
	f := store.Filter{Host: a.Host, Limit: 20}
	if a.Limit > 0 {
		f.Limit = a.Limit
	}
	if f.Limit > 100 {
		f.Limit = 100
	}
	if a.Cwd != "" {
		abs, err := filepath.Abs(a.Cwd)
		if err != nil {
			return f, err
		}
		f.Cwd = filepath.Clean(abs)
	}
	switch a.Exit {
	case "":
	case "fail", "nonzero":
		f.Failed = true
	default:
		n, err := strconv.ParseInt(a.Exit, 10, 64)
		if err != nil {
			return f, fmt.Errorf("exit: want a number or \"fail\", got %q", a.Exit)
		}
		f.Exit, f.ExitSet = n, true
	}
	if a.Since != "" {
		t, err := parseTimeSpec(a.Since)
		if err != nil {
			return f, fmt.Errorf("since: %w", err)
		}
		f.Since = t
	}
	if a.Until != "" {
		t, err := parseTimeSpec(a.Until)
		if err != nil {
			return f, fmt.Errorf("until: %w", err)
		}
		f.Until = t
	}
	return f, nil
}

func (s *mcpServer) red(text string) string {
	if !s.redact {
		return text
	}
	out, _ := redact.String(text, s.extra)
	return out
}

func (s *mcpServer) headline(c store.Command) string {
	from := ""
	if !c.Local() && c.Host != "" {
		from = " · from " + c.Host
	}
	took := ""
	if sp := fmtSpan(c); sp != "" {
		took = " · took " + sp
	}
	hist := ""
	if strings.HasPrefix(c.Machine, "hist:") {
		hist = "\n(imported from shell history — no output was recorded)"
	}
	return fmt.Sprintf("id %d · %s · cwd %s · exit %s%s · %s%s\n$ %s%s",
		c.ID, fmtWhen(c.StartedAt), c.Cwd, exitStr(c),
		took, humanBytes(c.OutputLen), from, s.red(c.Cmd), hist)
}

func (s *mcpServer) toolSearch(raw json.RawMessage) any {
	var a struct {
		Query        string `json:"query"`
		ContextLines *int   `json:"context_lines"`
		mcpFilterArgs
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("bad arguments: %v", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return errResult("query is required")
	}
	f, err := a.filter()
	if err != nil {
		return errResult("%v", err)
	}
	// trigram tokenizer: quote so natural strings (hyphens, dots, FTS
	// operators) search literally — same treatment as `backscroll search`.
	q := `"` + strings.ReplaceAll(a.Query, `"`, `""`) + `"`
	cmds, err := s.st.Search(q, f)
	if err != nil {
		return errResult("search failed: %v", err)
	}
	if len(cmds) == 0 {
		return textResult("no recorded commands matched " + strconv.Quote(a.Query))
	}
	ctxLines := -1
	if a.ContextLines != nil && *a.ContextLines >= 0 {
		ctxLines = *a.ContextLines
		if ctxLines > 10 {
			ctxLines = 10
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es), most recent first:\n\n", len(cmds))
	for _, c := range cmds {
		b.WriteString(s.headline(c))
		if ctxLines >= 0 {
			s.writeSearchContext(&b, c, a.Query, ctxLines)
		} else if snip := strings.TrimSpace(string(ansi.Strip([]byte(c.Snippet)))); snip != "" {
			for _, ln := range strings.Split(snip, "\n") {
				b.WriteString("\n    " + s.red(ln))
			}
		}
		b.WriteString("\n\n")
	}
	b.WriteString("Use get_output with an id to read a full output.")
	return textResult(b.String())
}

// writeSearchContext appends grep -C style hunks (line-numbered, ':' on
// matching lines, '-' on context lines) from the command's stored plain
// output. Redaction runs on the whole text before matching.
func (s *mcpServer) writeSearchContext(b *strings.Builder, c store.Command, query string, n int) {
	const maxHunks = 5
	text, err := s.st.Plain(c.ID)
	if err != nil || text == "" {
		return
	}
	text = s.red(text)
	hunks, shown, total := grepContext(text, query, n, n, maxHunks)
	for hi, h := range hunks {
		if hi > 0 {
			b.WriteString("\n    --")
		}
		for i, ln := range h.Lines {
			sep := "-"
			if h.IsMatch[i] {
				sep = ":"
			}
			fmt.Fprintf(b, "\n    %d%s%s", h.Start+i+1, sep, clipLine(ln, matchOffset(ln, query), 500))
		}
	}
	if total > shown {
		fmt.Fprintf(b, "\n    (+%d more matching lines — get_output id %d for everything)", total-shown, c.ID)
	}
}

func (s *mcpServer) toolList(raw json.RawMessage) any {
	var a mcpFilterArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("bad arguments: %v", err)
	}
	f, err := a.filter()
	if err != nil {
		return errResult("%v", err)
	}
	cmds, err := s.st.List(f)
	if err != nil {
		return errResult("list failed: %v", err)
	}
	if len(cmds) == 0 {
		return textResult("no recorded commands matched the filters")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d command(s), most recent first:\n\n", len(cmds))
	for _, c := range cmds {
		b.WriteString(s.headline(c))
		b.WriteString("\n\n")
	}
	b.WriteString("Use get_output with an id to read a full output.")
	return textResult(b.String())
}

const mcpDefaultMaxBytes = 50 << 10

func (s *mcpServer) toolGet(raw json.RawMessage) any {
	var a struct {
		ID       *int64 `json:"id"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("bad arguments: %v", err)
	}
	if a.ID == nil {
		return errResult("id is required (use -1 for the most recent command)")
	}
	if a.MaxBytes <= 0 {
		a.MaxBytes = mcpDefaultMaxBytes
	}
	c, err := s.st.Get(*a.ID)
	if err != nil {
		return errResult("not found (%v)", err)
	}
	out := ansi.Strip(c.Output)
	if s.redact {
		out, _ = redact.Bytes(out, s.extra)
	}
	note := ""
	if c.Truncated {
		note = "\n[note: output exceeded the recorder's cap; the middle was dropped at record time]"
	}
	body := string(out)
	if len(body) > a.MaxBytes {
		head := a.MaxBytes * 2 / 5
		tail := a.MaxBytes - head
		omitted := len(body) - head - tail
		// cut on line boundaries where possible
		h := body[:head]
		if i := strings.LastIndexByte(h, '\n'); i > 0 {
			h = h[:i+1]
		}
		t := body[len(body)-tail:]
		if i := strings.IndexByte(t, '\n'); i >= 0 && i < len(t)-1 {
			t = t[i+1:]
		}
		body = fmt.Sprintf("%s\n[... %d bytes omitted — call get_output with a larger max_bytes for more ...]\n%s",
			h, omitted, t)
	}
	return textResult(s.headline(*c) + note + "\n---\n" + body)
}

func (s *mcpServer) toolDiff(raw json.RawMessage) any {
	var a struct {
		ID      *int64 `json:"id"`
		Other   *int64 `json:"other"`
		Context *int   `json:"context"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("bad arguments: %v", err)
	}
	if a.ID == nil {
		return errResult("id is required (use -1 for the most recent command)")
	}
	newer, err := s.st.Get(*a.ID)
	if err != nil {
		return errResult("not found (%v)", err)
	}
	var older *store.Command
	if a.Other != nil {
		older, err = s.st.Get(*a.Other)
		if err != nil {
			return errResult("other: not found (%v)", err)
		}
	} else {
		older, err = s.st.PrevSame(newer.ID, newer.Cmd)
		if err != nil {
			return errResult("no previous run of %q found", s.red(newer.Cmd))
		}
	}
	ctx := 3
	if a.Context != nil && *a.Context >= 0 {
		ctx = *a.Context
	}
	label := func(c *store.Command) string {
		return fmt.Sprintf("#%d $ %s  (%s, exit %s)",
			c.ID, s.red(c.Cmd), fmtWhen(c.StartedAt), exitStr(*c))
	}
	oldOut := string(ansi.Strip(older.Output))
	newOut := string(ansi.Strip(newer.Output))
	if s.redact {
		oldOut = s.red(oldOut)
		newOut = s.red(newOut)
	}
	u := diff.Unified(diff.Lines(oldOut, newOut), label(older), label(newer), ctx, false)
	if u == "" {
		return textResult(fmt.Sprintf("outputs of #%d and #%d are identical", older.ID, newer.ID))
	}
	const cap = 200 << 10
	if len(u) > cap {
		u = u[:cap] + fmt.Sprintf("\n[... diff truncated at %d bytes ...]", cap)
	}
	return textResult(u)
}
