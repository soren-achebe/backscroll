#!/bin/sh
# Assemble backscroll-<version>.mcpb (MCP Bundle) from per-platform binaries.
#
# Usage:
#   mcpb/build.sh <version> <binaries-dir> <out.mcpb>
#
# <binaries-dir> must contain:
#   backscroll-linux-amd64  backscroll-linux-arm64
#   backscroll-darwin-amd64 backscroll-darwin-arm64
#
# The bundle is a zip:
#   manifest.json
#   server/run.sh
#   server/backscroll-<os>-<arch> (x4)
set -eu

ver=$1
bindir=$2
out=$3
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

for t in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  [ -f "$bindir/backscroll-$t" ] || { echo "missing $bindir/backscroll-$t" >&2; exit 1; }
done

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

mkdir -p "$stage/server"
sed "s/__VERSION__/$ver/" "$here/manifest.template.json" > "$stage/manifest.json"
cp "$here/run.sh" "$stage/server/run.sh"
chmod 755 "$stage/server/run.sh"
for t in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  cp "$bindir/backscroll-$t" "$stage/server/backscroll-$t"
  chmod 755 "$stage/server/backscroll-$t"
done

# sanity: manifest is valid JSON and version got substituted
python3 - "$stage/manifest.json" "$ver" <<'EOF'
import json, sys
m = json.load(open(sys.argv[1]))
assert m["version"] == sys.argv[2], m["version"]
EOF

rm -f "$out"
outabs=$(CDPATH='' cd -- "$(dirname -- "$out")" && pwd)/$(basename "$out")
(cd "$stage" && zip -q -r -X "$outabs" manifest.json server)
echo "built $out"
unzip -l "$out"
