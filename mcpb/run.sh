#!/bin/sh
# MCPB launcher for backscroll's MCP server.
# Invoked as: /bin/sh run.sh [extra args...]  (no exec bit required on this file)
# Picks the right bundled binary for this OS/arch and execs `backscroll mcp`.
set -e

dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) echo "backscroll mcpb: unsupported OS: $os" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "backscroll mcpb: unsupported arch: $arch" >&2; exit 1 ;;
esac

bin="$dir/backscroll-$os-$arch"
if [ ! -f "$bin" ]; then
  echo "backscroll mcpb: bundled binary not found: $bin" >&2
  exit 1
fi

# Zip extraction does not always preserve the exec bit; fix it up.
if [ ! -x "$bin" ]; then
  chmod +x "$bin" 2>/dev/null || true
fi
if [ ! -x "$bin" ]; then
  # Extension dir may be read-only: fall back to a cached copy.
  cache="${XDG_CACHE_HOME:-$HOME/.cache}/backscroll/mcpb"
  mkdir -p "$cache"
  cp "$bin" "$cache/backscroll-$os-$arch" && chmod +x "$cache/backscroll-$os-$arch"
  bin="$cache/backscroll-$os-$arch"
fi

# macOS: best-effort quarantine strip so Gatekeeper doesn't block the
# unsigned binary that arrived inside the downloaded bundle.
if [ "$os" = darwin ] && command -v xattr >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "$bin" 2>/dev/null || true
fi

exec "$bin" mcp "$@"
