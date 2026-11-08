#!/bin/sh
# SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
# SPDX-License-Identifier: GPL-3.0-or-later

# Builds both Windows versions, as the build workflow does: dist/relay-portable.exe
# and dist/relay-setup.exe. The installer holds relay.exe, so that isn't kept itself.
# Go runs in a container through scripts/go.sh.
#
#   scripts/build-windows.sh [version]
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"

version=${1:-dev-$(git rev-parse --short HEAD)}
flags="-s -w -H=windowsgui -X main.version=$version"

scripts/go.sh windows build -trimpath -ldflags "$flags -X main.portable=true" -o dist/relay-portable.exe ./cmd/relay

trap 'rm -f dist/relay.exe' EXIT
scripts/go.sh windows build -trimpath -ldflags "$flags" -o dist/relay.exe ./cmd/relay
scripts/build-installer.sh "$version"
