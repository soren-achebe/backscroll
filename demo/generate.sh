#!/bin/bash
# Regenerates demo/demo.cast + demo/demo.gif.
# Needs: asciinema, agg, go. Uses a sandboxed HOME (/tmp/demohome) and fake
# `make`/`healthcheck` commands (/tmp/demobin) so nothing real leaks in.
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p /tmp/demohome/project /tmp/demobin
go build -o /tmp/demobin/backscroll .

cat > /tmp/demobin/make <<'EOF'
#!/bin/bash
sleep 0.7
echo "building api image..."
sleep 0.3
echo "pushing registry.local/api:9f3ab12"
sleep 0.4
echo "ERROR: permission denied for role 'deploy'"
echo "make: *** [Makefile:6: deploy] Error 1"
exit 2
EOF

cat > /tmp/demobin/healthcheck <<'EOF'
#!/bin/bash
sleep 0.4
if [ ! -f /tmp/demohome/.hc_state ]; then
  touch /tmp/demohome/.hc_state
  cat <<'OUT'
api        ok        42ms
worker     ok        11ms
db         DEGRADED  replica lag 34s
cache      ok         2ms
OUT
else
  cat <<'OUT'
api        ok        38ms
worker     ok        12ms
db         ok        replica lag 0s
cache      ok         2ms
OUT
fi
EOF
chmod +x /tmp/demobin/make /tmp/demobin/healthcheck

cat > /tmp/demohome/.bashrc <<'EOF'
PS1='❯ '
export PATH=/tmp/demobin:$PATH
if [ -n "$BACKSCROLL_ACTIVE" ] && [ -z "$BACKSCROLL_HOOKED" ]; then
  eval "$(backscroll init bash)"
fi
EOF

# fresh state every take
rm -rf /tmp/demohome/.local /tmp/demohome/.config /tmp/demohome/.hc_state

# fake atuin history for the import + stats finale
python3 demo/gen-atuin.py /tmp/demohome/.local/share/atuin/history.db

asciinema rec --overwrite --cols 100 --rows 28 \
  -c "python3 demo/driver.py" demo/demo.cast
agg --theme dracula --font-size 16 --speed 1.0 demo/demo.cast demo/demo.gif
echo "done: demo/demo.cast demo/demo.gif"
