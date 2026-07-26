#!/bin/sh
# backscroll installer — https://github.com/soren-achebe/backscroll
#
#   curl -fsSL https://raw.githubusercontent.com/soren-achebe/backscroll/main/install.sh | sh
#
# Environment overrides:
#   BACKSCROLL_INSTALL_DIR  install directory   (default: ~/.local/bin)
#   BACKSCROLL_VERSION      release tag, e.g. v0.4.0   (default: latest)
#
# The script downloads the release tarball for your OS/arch, verifies its
# sha256 against the release's checksums.txt, and installs the binary (plus
# the man page, if a manpath is available). No sudo, no system files.

set -eu

REPO="soren-achebe/backscroll"
INSTALL_DIR="${BACKSCROLL_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${BACKSCROLL_VERSION:-latest}"

say()  { printf '%s\n' "$*"; }
fail() { printf 'install.sh: error: %s\n' "$*" >&2; exit 1; }

# --- platform detection ------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) fail "unsupported OS: $os (backscroll ships linux and darwin binaries)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail "unsupported architecture: $arch (amd64 and arm64 are available)" ;;
esac

asset="backscroll_${os}_${arch}.tar.gz"

if [ "$VERSION" = "latest" ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

# --- fetch -------------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  fail "need curl or wget"
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/backscroll-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $asset ($VERSION)..."
fetch "$base/$asset" "$tmp/$asset" \
  || fail "download failed: $base/$asset"
fetch "$base/checksums.txt" "$tmp/checksums.txt" \
  || fail "download failed: $base/checksums.txt"

# --- verify ------------------------------------------------------------------
want=$(awk -v a="$asset" '$2 == a { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "$asset not found in checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
else
  fail "need sha256sum or shasum to verify the download"
fi
[ "$got" = "$want" ] || fail "sha256 mismatch for $asset
  expected: $want
  got:      $got"
say "Checksum OK."

# --- install -----------------------------------------------------------------
tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/backscroll" ] || fail "tarball did not contain a backscroll binary"

mkdir -p "$INSTALL_DIR"
install_bin="$INSTALL_DIR/backscroll"
cp "$tmp/backscroll" "$install_bin.new.$$"
chmod 755 "$install_bin.new.$$"
mv -f "$install_bin.new.$$" "$install_bin"   # atomic: safe if backscroll is running

# man page (best effort)
if [ -f "$tmp/man/backscroll.1" ]; then
  man_dir="${XDG_DATA_HOME:-$HOME/.local/share}/man/man1"
  mkdir -p "$man_dir" 2>/dev/null && cp "$tmp/man/backscroll.1" "$man_dir/" 2>/dev/null \
    && say "Man page installed to $man_dir/backscroll.1"
fi

say ""
say "Installed: $install_bin"
"$install_bin" --version 2>/dev/null || true

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    say ""
    say "NOTE: $INSTALL_DIR is not in your PATH. Add this to your shell rc:"
    say "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

say ""
say "Next: enable recording (see 'Set up' in the README):"
say "  bash: echo 'eval \"\$(backscroll init bash)\"' >> ~/.bashrc"
say "  zsh:  echo 'eval \"\$(backscroll init zsh)\"'  >> ~/.zshrc"
say "  fish: echo 'backscroll init fish | source'    >> ~/.config/fish/config.fish"
say "then start a recorded shell with:  backscroll run"
