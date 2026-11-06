#!/bin/sh
# SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
# SPDX-License-Identifier: GPL-3.0-or-later

# Runs Go in a container, because the host doesn't have it. The container sees only
# this repo and Go's cache folder.
#
#   scripts/go.sh test -race ./...
#   scripts/go.sh gofmt -l .
#   scripts/go.sh windows build ./...     (for Windows)
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=golang:$(sed -n 's/^go \([0-9]*\.[0-9]*\).*/\1/p' "$repo/go.mod")
cache="$HOME/.cache/fdc-relay-go"
mkdir -p "$cache"

target=""
if [ "${1:-}" = windows ]; then
	shift
	target="-e GOOS=windows -e GOARCH=amd64 -e CGO_ENABLED=0"
fi

tool=go
if [ "${1:-}" = gofmt ]; then
	shift
	tool=gofmt
fi

# shellcheck disable=SC2086
exec docker run --rm -v "$repo":/src -w /src -u "$(id -u):$(id -g)" -v "$cache":/cache \
	-e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod $target \
	"$image" "$tool" "$@"
