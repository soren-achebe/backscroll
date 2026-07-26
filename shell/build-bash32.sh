#!/bin/sh
# Build bash 3.2.57 (the macOS /bin/bash vintage) into a prefix, for
# running shell/test_bash_integration.py against the pre-4.0 fallback
# widget guard. Usage: shell/build-bash32.sh [prefix]   (needs gcc, bison)
set -eu

PREFIX=${1:-"$HOME/.cache/bash-3.2.57"}
if [ -x "$PREFIX/bin/bash" ]; then
    echo "bash 3.2.57 already present at $PREFIX/bin/bash"
    exit 0
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"

curl -fsSLO https://ftp.gnu.org/gnu/bash/bash-3.2.57.tar.gz
echo "3fa9daf85ebf35068f090ce51283ddeeb3c75eb5bc70b1a4a7cb05868bfe06a4  bash-3.2.57.tar.gz" | sha256sum -c -
tar xzf bash-3.2.57.tar.gz
cd bash-3.2.57

./configure --prefix="$PREFIX" >configure.log 2>&1
# The shipped y.tab.c is stale (generated from a different parse.y) and
# fails against it; regenerate with bison instead.
rm -f y.tab.c y.tab.h
make y.tab.c YACC="bison -y" >yacc.log 2>&1
make -j"$(nproc 2>/dev/null || echo 2)" >make.log 2>&1 || { tail -30 make.log; exit 1; }
make install >install.log 2>&1 || { tail -30 install.log; exit 1; }

"$PREFIX/bin/bash" --version | head -1
