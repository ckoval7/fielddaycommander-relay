#!/bin/sh
# SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
# SPDX-License-Identifier: GPL-3.0-or-later

# Builds dist/relay-setup.exe from dist/relay.exe. It uses makensis if it's installed,
# or a container if it isn't.
#
#   scripts/build-installer.sh [version]
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"

version=${1:-dev}
# Windows wants four numbers. Anything but a release, like dev-abc1234, has none.
case $version in
	[0-9]*.[0-9]*.[0-9]*) numeric=$version.0 ;;
	*) numeric=0.0.0.0 ;;
esac

if [ ! -f dist/relay.exe ]; then
	echo "Build dist/relay.exe first." >&2
	exit 1
fi

# -NOCD keeps paths relative to the repo, not to the script.
set -- -V2 -NOCD "-DVERSION=$version" "-DNUMERIC_VERSION=$numeric" \
	-DRELAY_EXE=dist/relay.exe -DOUT_FILE=dist/relay-setup.exe installer/relay.nsi

if command -v makensis >/dev/null 2>&1; then
	exec makensis "$@"
fi
exec docker run --rm -v "$repo":/src -w /src ubuntu:24.04 sh -c '
	set -e
	apt-get update -qq >/dev/null
	apt-get install -y -qq --no-install-recommends nsis >/dev/null
	owner=$1
	shift
	makensis "$@"
	chown "$owner" dist/relay-setup.exe
' sh "$(id -u):$(id -g)" "$@"
