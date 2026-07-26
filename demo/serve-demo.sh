#!/bin/bash
# Reproduces the database behind demo/serve.png: records a small, sanitized
# bash session (fake HOME, no real user data) into $DB, then serves it.
#
#   ./demo/serve-demo.sh /path/to/backscroll
#
# Screenshot: open http://127.0.0.1:4133/, expand the second ~/health.sh
# run, click "diff vs previous run". Viewport 1200x760.
set -euo pipefail

BKS=${1:?usage: serve-demo.sh /path/to/backscroll}
DB=$(mktemp /tmp/bks-serve-demo-XXXX.db)
H=$(mktemp -d /tmp/bks-serve-home-XXXX)
mkdir -p "$H/src/webapp" && (cd "$H/src/webapp" && git init -q . && echo 'package main' > main.go && git add .)
cat > "$H/health.sh" <<'EOF'
#!/bin/bash
if [ -f "$HOME/.degraded" ]; then
  printf 'api      : \033[31mDEGRADED\033[0m\nlatency  : 941ms\nqueue    : 8812 jobs\n'
else
  printf 'api      : \033[32mok\033[0m\nlatency  : 12ms\nqueue    : 3 jobs\n'
fi
EOF
chmod +x "$H/health.sh"

python3 - "$BKS" "$DB" "$H" <<'EOF'
import pexpect, os, sys, time
bks, db, home = sys.argv[1:4]
env = dict(BACKSCROLL_DB=db, TERM="xterm-256color", SHELL="/bin/bash",
           HOME=home, PATH=os.environ["PATH"], LANG="C.UTF-8")
c = pexpect.spawn(bks, ["run"], env=env, cwd=home + "/src/webapp",
                  encoding=None, dimensions=(30, 110), timeout=20)
c.expect(r"\$")
def run(cmd, wait=0.7):
    c.sendline(cmd); time.sleep(wait); c.expect(r"\$", timeout=25)
run('eval "$(%s init bash)"' % bks); time.sleep(0.4)
run("ls --color=always /etc | head -8")
run("git status")
run("curl -s --max-time 2 http://127.0.0.1:9/api/health || echo 'curl: (7) Failed to connect to 127.0.0.1 port 9: Connection refused'")
run("~/health.sh")
run("df -h /")
run("touch ~/.degraded")
run("~/health.sh")
run("false")
c.sendline("exit"); c.expect(pexpect.EOF, timeout=10)
EOF

echo "demo DB: $DB"
exec env BACKSCROLL_DB="$DB" "$BKS" serve
