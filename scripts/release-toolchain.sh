#!/bin/sh
# Prints the newest patch release of Go <minor> (1.27 -> go1.27.N), from
# go.dev's list of supported releases. `make release` builds under GOTOOLCHAIN
# set to it, so shipped binaries carry the latest patch whatever Go the releaser
# has installed; the go command downloads that toolchain and checks it against
# the Go checksum database. A minor that has dropped out of support lists no
# release, and the script fails rather than build on an unpatched one: move
# go.mod's go directive to a supported minor.
set -eu

minor="${1:?usage: release-toolchain.sh <go minor, e.g. 1.27>}"
pattern="\"go$(printf '%s' "$minor" | sed 's/\./\\./g')\\.[0-9]+\""

# Fetched apart from the parse, so a network failure isn't reported as an
# unsupported minor: a pipeline's status is its last command's.
if ! releases="$(curl -fsSL --max-time 30 --retry 2 --proto '=https' 'https://go.dev/dl/?mode=json')"; then
	echo "release-toolchain: could not fetch the Go release list from go.dev" >&2
	exit 1
fi
latest="$(printf '%s' "$releases" | grep -oE "$pattern" | tr -d '"' | sort -t. -k3,3n | tail -n 1)" || true

if [ -z "$latest" ]; then
	echo "release-toolchain: go.dev lists no supported go${minor}.x release" >&2
	exit 1
fi
echo "$latest"
