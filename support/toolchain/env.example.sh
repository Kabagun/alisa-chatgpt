#!/bin/sh
# Source this from POSIX sh after copying it to ignored env.local.sh.
# Point SUPPORT_ROOT to this support directory; no installation or device access.
: "${SUPPORT_ROOT:?Set SUPPORT_ROOT to the absolute support source directory}"
: "${GO:=go}"
: "${CC:=cc}"
command -v "$GO" >/dev/null || return 1
command -v "$CC" >/dev/null || return 1
export GO CC
export GOENV=off GOTOOLCHAIN=local GOTELEMETRY=off GOWORK=off
export GOPROXY=off GOSUMDB=off
export GOCACHE="$SUPPORT_ROOT/temp/go-build-cache"
export GOMODCACHE="$SUPPORT_ROOT/temp/go-module-cache"
export GOTMPDIR="$SUPPORT_ROOT/temp/go-tmp"
export TMPDIR="$SUPPORT_ROOT/temp/tmp"
export PYTHONDONTWRITEBYTECODE=1
mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOTMPDIR" "$TMPDIR"
