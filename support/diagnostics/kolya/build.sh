#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
: "${CC:=cc}"
command -v "$CC" >/dev/null
mkdir -p "$ROOT/build"
"$CC" -std=c11 -O2 -Wall -Wextra -Werror "$ROOT/model-probe.c" -ldl -o "$ROOT/build/model-probe"
