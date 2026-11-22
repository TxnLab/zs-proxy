#!/usr/bin/env bash
#
# Code-sign a macOS binary with a Developer ID (release) or self-signed dev
# identity. No-ops (exit 0) when not on macOS or MACOS_SIGN_IDENTITY is unset,
# so dev/snapshot builds and CI without signing secrets don't fail.
#
#   MACOS_SIGN_IDENTITY   codesign identity (e.g. "Developer ID Application: …",
#                         a self-signed cert CN, or "-" for ad-hoc).
#   SIGN_NO_TIMESTAMP=1   skip the secure timestamp (required for ad-hoc /
#                         self-signed certs, which can't use Apple's TS server).
set -euo pipefail

BIN="${1:?usage: sign-macos.sh <binary>}"
IDENTITY="${MACOS_SIGN_IDENTITY:-}"

[[ "$(uname -s)" == "Darwin" ]] || exit 0
if [[ -z "$IDENTITY" ]]; then
  echo "sign-macos: MACOS_SIGN_IDENTITY unset — skipping codesign of $BIN" >&2
  exit 0
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTITLEMENTS="$SCRIPT_DIR/../build/entitlements.plist"

TS="--timestamp"
if [[ "${SIGN_NO_TIMESTAMP:-}" == "1" || "$IDENTITY" == "-" ]]; then
  TS="--timestamp=none"
fi

codesign --force $TS --options runtime \
  --entitlements "$ENTITLEMENTS" \
  --sign "$IDENTITY" "$BIN"
codesign --verify --strict --verbose=2 "$BIN"
echo "sign-macos: ✔ signed $BIN with '$IDENTITY'" >&2
