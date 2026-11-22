#!/usr/bin/env bash
#
# Create a self-signed code-signing certificate in the login keychain so local
# dev builds get a STABLE signing identity. That's enough for macOS to trust the
# (signed) zerosignal binary's access to its own Keychain items and stop
# prompting on every run — no Apple Developer account required. Release builds
# use a real Developer ID instead.
#
#   ./scripts/dev-cert-macos.sh ["zerosignal Local Dev"]
#   MACOS_SIGN_IDENTITY="zerosignal Local Dev" SIGN_NO_TIMESTAMP=1 make signed
#   remove with:  security delete-identity -c "zerosignal Local Dev"
set -euo pipefail

NAME="${1:-zerosignal Local Dev}"
KEYCHAIN="${HOME}/Library/Keychains/login.keychain-db"

[[ "$(uname -s)" == "Darwin" ]] || { echo "dev-cert: macOS only" >&2; exit 1; }

# Note: no -v — a self-signed cert is untrusted (CSSMERR_TP_NOT_TRUSTED) and so
# is hidden by -v, but codesign still signs with it fine.
if security find-identity -p codesigning | grep -q "$NAME"; then
  echo "dev-cert: identity '$NAME' already present" >&2
  echo "  sign with:  MACOS_SIGN_IDENTITY='$NAME' SIGN_NO_TIMESTAMP=1 make signed" >&2
  exit 0
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/req.cfg" <<EOF
[req]
distinguished_name = dn
x509_extensions = v3
prompt = no
[dn]
CN = $NAME
[v3]
basicConstraints = critical,CA:false
keyUsage = critical,digitalSignature
extendedKeyUsage = critical,codeSigning
EOF

openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout "$TMP/key.pem" -out "$TMP/cert.pem" -config "$TMP/req.cfg" >/dev/null 2>&1

# macOS `security import` can't verify OpenSSL 3's default SHA-256 PKCS12 MAC,
# so on OpenSSL 3 emit a legacy (3DES/RC2 + SHA1-MAC) bundle. LibreSSL (older
# default in PATH on some Macs) already produces a compatible bundle.
P12_ARGS=()
if openssl version | grep -q "^OpenSSL 3"; then
  P12_ARGS=(-legacy -macalg sha1)
fi
openssl pkcs12 -export "${P12_ARGS[@]}" -inkey "$TMP/key.pem" -in "$TMP/cert.pem" \
  -out "$TMP/id.p12" -passout pass:zerosignal -name "$NAME" >/dev/null 2>&1

# Import the cert+key and pre-authorize codesign to use the private key so
# signing doesn't prompt for the key on every run.
security import "$TMP/id.p12" -k "$KEYCHAIN" -P zerosignal -T /usr/bin/codesign >/dev/null

echo "dev-cert: ✔ created self-signed code-signing identity '$NAME'" >&2
echo "  build a signed binary:  MACOS_SIGN_IDENTITY='$NAME' SIGN_NO_TIMESTAMP=1 make signed" >&2
echo "  remove later:           security delete-identity -c '$NAME'" >&2
echo >&2
echo "  NOTE: a self-signed cert is for LOCAL dev only — it is not notarizable" >&2
echo "        and other machines will still warn. Releases use a Developer ID." >&2
