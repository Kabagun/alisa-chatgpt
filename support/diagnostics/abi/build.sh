#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
: "${CC:=cc}"
: "${STATION_NATIVE_DIR:=$ROOT/../../../station/internal/voice}"
command -v "$CC" >/dev/null
[ -f "$STATION_NATIVE_DIR/station_native.h" ]
[ -f "$STATION_NATIVE_DIR/station_native.c" ]
mkdir -p "$ROOT/build"
"$CC" -std=c11 -O2 -Wall -Wextra -Werror -I"$STATION_NATIVE_DIR" "$ROOT/spotter-abi.c" "$STATION_NATIVE_DIR/station_native.c" -ldl -o "$ROOT/build/spotter-abi"
