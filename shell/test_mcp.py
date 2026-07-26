#!/usr/bin/env python3
r"""End-to-end test: `backscroll mcp` — the Model Context Protocol server.

Records a real bash session through `backscroll run` on a PTY, then talks
real newline-delimited JSON-RPC to `backscroll mcp` over stdio, the way an
MCP client (Claude Code, Cursor, VS Code, ...) would:

  1. initialize handshake: protocol version echoed, capabilities.tools
     advertised, serverInfo.name = backscroll
  2. notifications get no response (initialized)
  3. tools/list: all four tools with schemas
  4. search_output finds a command by a string its OUTPUT printed —
     including hyphenated queries (FTS quoting) — and never leaks secrets
     into snippets
  5. list_commands with exit=fail returns only the failure
  6. get_output by relative id; secrets in outputs are redacted by DEFAULT,
     surrounding text survives
  7. get_output max_bytes truncation keeps head + tail with a gap marker
  8. diff_output vs the previous run of the same command; missing-prev is
     an in-band tool error (isError), not a protocol error
  9. unknown method -> JSON-RPC -32601; unknown tool -> isError
 10. clean shutdown on stdin EOF, nothing ever written to stdout except
     protocol frames (every line parses as JSON-RPC)

Env:
  BKS_BIN   path to a built backscroll binary (default: ./backscroll)
"""

import json
import os
import subprocess
import sys
import tempfile
import time

import pexpect

BKS = os.path.abspath(os.environ.get("BKS_BIN", "./backscroll"))

checks = 0


def ok(cond, label, detail=""):
    global checks
    checks += 1
    if cond:
        print(f"  ok {checks:2d} - {label}")
    else:
        print(f"FAIL {checks:2d} - {label}\n{detail}")
        sys.exit(1)


def record_session(home, env):
    with open(os.path.join(home, ".bashrc"), "w") as f:
        f.write(f'eval "$({BKS} init bash)"\nPS1="$ "\n')
    sh = pexpect.spawn(BKS, ["run"], env=env, timeout=15)
    sh.expect(rb"\$ ")

    def run(cmd):
        sh.sendline(cmd)
        sh.expect(rb"\$ ")

    run("echo hello-from-pty")            # hyphenated needle, output search
    run("printf 'attempt one\\nshared line\\n' > out.txt")
    run("cat out.txt")
    run("printf 'attempt two\\nshared line\\n' > out.txt")
    run("cat out.txt")                    # diff target vs previous cat
    run("echo token is ghp_ZZZZefghijklmnopqrstuvwxyz0123456789")
    run("false")                          # the only failure
    sh.sendline("exit")
    sh.expect(pexpect.EOF)
    sh.wait()
    time.sleep(0.3)


class Client:
    def __init__(self, env):
        self.p = subprocess.Popen(
            [BKS, "mcp"], env=env, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.next_id = 0

    def send(self, obj):
        self.p.stdin.write((json.dumps(obj) + "\n").encode())
        self.p.stdin.flush()

    def rpc(self, method, params=None):
        self.next_id += 1
        req = {"jsonrpc": "2.0", "id": self.next_id, "method": method}
        if params is not None:
            req["params"] = params
        self.send(req)
        line = self.p.stdout.readline()
        resp = json.loads(line)  # every stdout line must be valid JSON
        assert resp.get("id") == self.next_id, (resp, self.next_id)
        return resp

    def tool(self, name, args):
        resp = self.rpc("tools/call", {"name": name, "arguments": args})
        res = resp["result"]
        assert len(res["content"]) == 1 and res["content"][0]["type"] == "text", res
        return res["content"][0]["text"], res.get("isError", False)


def main():
    home = tempfile.mkdtemp(prefix="bks-mcp-test")
    env = dict(os.environ, HOME=home, TERM="xterm-256color", SHELL="/bin/bash",
               BACKSCROLL_DB=os.path.join(home, "db.sqlite"))
    for k in [k for k in env if k.startswith("XDG_")]:
        del env[k]
    record_session(home, env)

    c = Client(env)

    # 1. handshake
    resp = c.rpc("initialize", {"protocolVersion": "2025-03-26",
                                "capabilities": {},
                                "clientInfo": {"name": "e2e", "version": "0"}})
    init = resp["result"]
    ok(init["protocolVersion"] == "2025-03-26", "protocol version echoed")
    ok("tools" in init["capabilities"] and init["serverInfo"]["name"] == "backscroll",
       "capabilities.tools + serverInfo", json.dumps(init))

    # 2. notification: no response expected — verify by pinging right after
    c.send({"jsonrpc": "2.0", "method": "notifications/initialized"})
    resp = c.rpc("ping")
    ok(resp.get("result") == {}, "notification silent; ping answered next", resp)

    # 3. tools/list
    tools = c.rpc("tools/list")["result"]["tools"]
    names = [t["name"] for t in tools]
    ok(names == ["search_output", "list_commands", "get_output", "diff_output"],
       "tools/list has the four tools", names)
    ok(all(t["description"] and t["inputSchema"]["type"] == "object" for t in tools),
       "every tool has description + object schema")

    # 4. search by output content, hyphenated query
    text, is_err = c.tool("search_output", {"query": "hello-from-pty"})
    ok(not is_err and "$ echo hello-from-pty" in text,
       "search_output finds command by its output (hyphenated query)", text)
    text, is_err = c.tool("search_output", {"query": "token is"})
    ok(not is_err and "ghp_ZZZZefghijklmnopqrstuvwxyz0123456789" not in text,
       "search snippets are redacted", text)

    # 5. list failures only
    text, is_err = c.tool("list_commands", {"exit": "fail"})
    ok(not is_err and "$ false" in text and "1 command(s)" in text,
       "list_commands exit=fail returns only the failure", text)

    # 6. get_output relative id + default redaction
    text, is_err = c.tool("get_output", {"id": -2})  # -1=false, -2=echo token
    ok(not is_err and "token is" in text
       and "ghp_ZZZZefghijklmnopqrstuvwxyz0123456789" not in text,
       "get_output redacts secrets by default, keeps context", text)

    # 7. max_bytes truncation (head + tail + marker)
    text, is_err = c.tool("get_output", {"id": -7, "max_bytes": 8})
    ok(not is_err and "bytes omitted" in text,
       "get_output max_bytes truncates with a gap marker", text)

    # 8. diff vs previous run of same command
    text, is_err = c.tool("diff_output", {"id": -3})  # second cat out.txt
    ok(not is_err and "-attempt one" in text and "+attempt two" in text
       and " shared line" in text,
       "diff_output vs previous run of the same command", text)
    text, is_err = c.tool("diff_output", {"id": -1})  # false: only run
    ok(is_err and "no previous run" in text,
       "diff_output missing-prev is an in-band tool error", text)

    # 9. protocol errors
    resp = c.rpc("definitely/not-a-method")
    ok(resp.get("error", {}).get("code") == -32601, "unknown method -> -32601", resp)
    text, is_err = c.tool("nope", {})
    ok(is_err and "unknown tool" in text, "unknown tool -> isError", text)

    # 10. clean shutdown on EOF
    c.p.stdin.close()
    rc = c.p.wait(timeout=5)
    stderr = c.p.stderr.read().decode()
    ok(rc == 0, "clean exit on stdin EOF", f"rc={rc} stderr={stderr[:300]}")

    print(f"\nall {checks} checks passed")


if __name__ == "__main__":
    main()
