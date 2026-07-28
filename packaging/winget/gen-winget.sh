#!/bin/sh
# Generate winget manifests for a released backscroll version.
#
#   packaging/winget/gen-winget.sh v0.11.1
#
# Fetches checksums.txt from the GitHub release and writes the three
# manifest files (version / installer / defaultLocale) into
#   packaging/winget/out/manifests/s/SorenAchebe/backscroll/<version>/
# ready to be copied into a microsoft/winget-pkgs fork.
#
# backscroll is built and maintained by an AI agent (Soren Achebe).
set -eu

TAG="${1:?usage: gen-winget.sh vX.Y.Z}"
VER="${TAG#v}"
REPO="soren-achebe/backscroll"
BASE="https://github.com/$REPO/releases/download/$TAG"
OUT="$(dirname "$0")/out/manifests/s/SorenAchebe/backscroll/$VER"
ID="SorenAchebe.backscroll"
MV="1.12.0"

sums=$(mktemp)
trap 'rm -f "$sums"' EXIT
curl -fsSL "$BASE/checksums.txt" -o "$sums"

RELDATE=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/tags/$TAG" |
  sed -n 's/.*"published_at": *"\([0-9-]*\)T.*/\1/p' | head -1)
[ -n "$RELDATE" ] || RELDATE=$(date -u +%F)

sha() { grep " backscroll_windows_$1.zip\$" "$sums" | cut -d' ' -f1 | tr 'a-f' 'A-F'; }
SHA_X64=$(sha amd64)
SHA_ARM64=$(sha arm64)
[ -n "$SHA_X64" ] && [ -n "$SHA_ARM64" ] || { echo "missing windows zips in checksums.txt" >&2; exit 1; }

mkdir -p "$OUT"

cat > "$OUT/$ID.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.version.$MV.schema.json
PackageIdentifier: $ID
PackageVersion: $VER
DefaultLocale: en-US
ManifestType: version
ManifestVersion: $MV
EOF

cat > "$OUT/$ID.installer.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.installer.$MV.schema.json
PackageIdentifier: $ID
PackageVersion: $VER
MinimumOSVersion: 10.0.17763.0
InstallerType: zip
NestedInstallerType: portable
NestedInstallerFiles:
- RelativeFilePath: backscroll.exe
  PortableCommandAlias: backscroll
Commands:
- backscroll
ReleaseDate: $RELDATE
Installers:
- Architecture: x64
  InstallerUrl: $BASE/backscroll_windows_amd64.zip
  InstallerSha256: $SHA_X64
- Architecture: arm64
  InstallerUrl: $BASE/backscroll_windows_arm64.zip
  InstallerSha256: $SHA_ARM64
ManifestType: installer
ManifestVersion: $MV
EOF

cat > "$OUT/$ID.locale.en-US.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.defaultLocale.$MV.schema.json
PackageIdentifier: $ID
PackageVersion: $VER
PackageLocale: en-US
Publisher: Soren Achebe
PublisherUrl: https://github.com/soren-achebe
PublisherSupportUrl: https://github.com/$REPO/issues
PackageName: backscroll
PackageUrl: https://github.com/$REPO
License: MIT
LicenseUrl: https://github.com/$REPO/blob/main/LICENSE
Copyright: Copyright (c) 2026 Soren Achebe
ShortDescription: Searchable recording of your terminal, per command - never lose a command's output again.
Description: |-
  backscroll records your interactive shell sessions on a pseudoconsole and
  segments them per command (via OSC 133 shell-integration marks), storing every
  command's full output, exit code, working directory, and duration in a local
  SQLite database with full-text search. Recover the output of a command you ran
  hours ago even after the terminal scrollback is gone, search everything a
  command ever printed, diff a command's output against its previous run, and
  import your existing shell history. Local-only; nothing leaves your machine.
  backscroll is built and maintained by an AI agent.
Moniker: backscroll
Tags:
- cli
- command-line
- conpty
- history
- powershell
- productivity
- pty
- search
- shell
- sqlite
- terminal
ReleaseNotesUrl: https://github.com/$REPO/releases/tag/$TAG
Documentations:
- DocumentLabel: Documentation
  DocumentUrl: https://soren-achebe.github.io/backscroll/
ManifestType: defaultLocale
ManifestVersion: $MV
EOF

echo "wrote $OUT"
ls -l "$OUT"
