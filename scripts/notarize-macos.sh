#!/usr/bin/env bash
#
# Notarize a Developer ID-signed artifact. notarytool only accepts .zip, .pkg,
# or .dmg — NOT .tar.gz — so when handed the goreleaser darwin tarball we extract
# the signed binary and repackage it as a .zip for submission.
#
# This is sound because notarization attaches to the binary's CODE SIGNATURE
# (cdhash), not to the container: once Apple notarizes the cdhash, the identical
# binary inside the distributed .tar.gz is notarized too. Gatekeeper verifies it
# ONLINE on first run (a loose .tar.gz/.zip can't carry a stapled ticket).
#
# Release-only — needs notarytool credentials stored as a keychain profile:
#   xcrun notarytool store-credentials zerosignal-notary \
#     --apple-id you@example.com --team-id TEAMID --password <app-specific-pw>
#
#   NOTARY_PROFILE   keychain profile name (default: zerosignal-notary)
set -euo pipefail

ARTIFACT="${1:?usage: notarize-macos.sh <tar.gz|zip|pkg|dmg>}"
PROFILE="${NOTARY_PROFILE:-zerosignal-notary}"

[[ "$(uname -s)" == "Darwin" ]] || { echo "notarize: macOS only" >&2; exit 1; }
[[ -f "$ARTIFACT" ]] || { echo "notarize: no such file: $ARTIFACT" >&2; exit 1; }

# notarytool rejects tarballs; repackage the signed binary as a .zip.
case "$ARTIFACT" in
  *.tar.gz | *.tgz)
    work="$(mktemp -d)"
    trap 'rm -rf "$work"' EXIT
    tar -xzf "$ARTIFACT" -C "$work"
    # First executable in the archive. No `| head` — under `set -o pipefail`
    # head closes the pipe early and SIGPIPEs find; take the first line with
    # parameter expansion instead.
    bins="$(find "$work" -type f -perm -u+x)"
    bin="${bins%%$'\n'*}"
    [[ -n "$bin" ]] || { echo "notarize: no executable found inside $ARTIFACT" >&2; exit 1; }

    # Fail early on an unsigned / non-hardened binary — Apple rejects it with a
    # less obvious error. Capture codesign's output and match with [[ ]] rather
    # than `| grep -q`: under `set -o pipefail`, grep -q closing the pipe the
    # instant it matches SIGPIPEs codesign, making the pipeline falsely report
    # failure on an otherwise-correctly-signed binary (a timing-dependent race).
    siginfo="$(codesign -dvv "$bin" 2>&1 || true)"
    [[ "$siginfo" == *"(runtime)"* ]] \
      || { echo "notarize: $(basename "$bin") is not hardened-runtime signed — goreleaser's sign hook didn't run (is MACOS_SIGN_IDENTITY set?); re-run 'make release'" >&2; exit 1; }

    base="${ARTIFACT%.tar.gz}"; base="${base%.tgz}"
    submit="${base}.zip"
    /usr/bin/ditto -c -k "$bin" "$submit"   # Apple-recommended zipper; preserves the signature
    echo "notarize: repackaged $(basename "$bin") -> $(basename "$submit")" >&2
    ;;
  *)
    submit="$ARTIFACT"
    ;;
esac

xcrun notarytool submit "$submit" --keychain-profile "$PROFILE" --wait
# Stapling needs a container (.pkg/.dmg/.app); a .zip or bare binary can't be
# stapled, so Gatekeeper verifies notarization online. Best-effort.
xcrun stapler staple "$submit" 2>/dev/null || true
echo "notarize: ✔ $submit (binary cdhash notarized — covers the distributed .tar.gz)" >&2
