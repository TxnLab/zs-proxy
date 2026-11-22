#!/bin/sh
#
# One-line installer for the ZeroSignal CLIs:
#
#   curl -fsSL https://zerosignal.ai/install.sh | sh                  # zs-proxy
#   curl -fsSL https://zerosignal.ai/install.sh | sh -s -- zs-node    # zs-node
#
# Unlike its siblings in this directory this is POSIX `sh`, not bash: it has to
# run on a stock Alpine/Debian container where /bin/sh is dash or busybox ash
# and bash may not exist at all.
#
# Two structural rules exist because this is piped into a shell:
#   1. Everything lives in a function and `main "$@"` is the LAST line, so a
#      TRUNCATED download defines a partial set of functions and exits without
#      running anything, instead of executing half an installer.
#   2. Nothing reads stdin — the script IS stdin. No prompts, no `read`.
#
# Trust model, stated honestly: checksums.txt is fetched over the same TLS
# connection as the archive and nothing signs it, so verification here defends
# against corruption, truncation, and a stale CDN object — NOT against a
# compromised release. Release signing (cosign/minisign with a pinned key) is a
# follow-up; note that making verification conditional on a tool being present
# would be worse than none, since an attacker just makes the tool look absent.
#
#   ZS_PRODUCT       zs-proxy (default) or zs-node; also the positional arg
#   ZS_VERSION       pin a release (1.2.3 or v1.2.3); skips the GitHub API,
#                    which is also the escape hatch from its 60/hr rate limit
#   ZS_INSTALL_DIR   target bin dir (default: see resolve_dir)
#   ZS_OS/ZS_ARCH    override uname detection
#   ZS_REPO          release repo (default txnlab/<product>)
#   ZS_GITHUB        download origin (default https://github.com)
#   ZS_GITHUB_API    API origin (default https://api.github.com)
#   ZS_FORCE=1       install even when Homebrew/Scoop already owns the binary
#   ZS_USE_SUDO=1    sudo ONLY the final install step (never the whole script)
#
# shellcheck disable=SC3043
# `local` is not in POSIX, but every /bin/sh we target implements it (dash,
# busybox ash, and macOS's bash-3.2 in sh mode). rustup-init.sh sets the same
# precedent. Without it every helper would leak its temporaries into globals.

set -eu
umask 022

DEFAULT_PRODUCT="zs-proxy"
GH="${ZS_GITHUB:-https://github.com}"
GH_API="${ZS_GITHUB_API:-https://api.github.com}"

# product is resolved in main() and read by the message helpers.
product="$DEFAULT_PRODUCT"

# tmpdir is global rather than local to main() because the EXIT trap fires after
# main() has returned, when a local would already be out of scope — under
# `set -u` that turns every clean exit into an "unbound variable" error.
tmpdir=""
cleanup() { [ -n "$tmpdir" ] && rm -rf "$tmpdir"; }

die() { printf '%s: %s\n' "$product" "$*" >&2; exit 1; }
info() { printf '%s\n' "$*"; }
warn() { printf '%s: warning: %s\n' "$product" "$*" >&2; }
have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- product ----

# resolve_product validates against a literal allow-list rather than sanitizing.
# The value is interpolated into a URL path, an archive member name, and a
# filesystem path, so "is it one of the two things we publish" is the only check
# that can't be talked around.
resolve_product() {
	local p="${1:-${ZS_PRODUCT:-$DEFAULT_PRODUCT}}"
	case "$p" in
	zs-proxy | zs-node) printf '%s' "$p" ;;
	*) die "unknown product: $p (expected zs-proxy or zs-node)" ;;
	esac
}

# ---------------------------------------------------------------- os/arch ----

detect_os() {
	if [ -n "${ZS_OS:-}" ]; then
		printf '%s' "$ZS_OS"
		return
	fi
	case "$(uname -s)" in
	Darwin) printf 'darwin' ;;
	Linux) printf 'linux' ;;
	MINGW* | MSYS* | CYGWIN* | Windows_NT)
		die "Windows installs with Scoop: scoop bucket add txnlab https://github.com/txnlab/scoop-bucket && scoop install $product"
		;;
	*) die "unsupported OS: $(uname -s)" ;;
	esac
}

detect_arch() {
	local m
	m="${ZS_ARCH:-$(uname -m)}"
	case "$m" in
	x86_64 | amd64) printf 'amd64' ;;
	aarch64 | arm64 | armv8*) printf 'arm64' ;;
	*) die "unsupported architecture: $m (we publish amd64 and arm64)" ;;
	esac
}

# asset_arch: goreleaser fuses both darwin slices into ONE universal archive
# (universal_binaries + replace: true), published as _darwin_all. There is no
# darwin_amd64/darwin_arm64 asset — asking for one 404s.
asset_arch() {
	if [ "$1" = darwin ]; then
		printf 'all'
	else
		printf '%s' "$2"
	fi
}

# ------------------------------------------------------------------ fetch ----

# --proto-redir constrains the REDIRECT hop (github.com to the objects host)
# to https. Plain --proto would additionally reject the origin, which breaks
# the test harness's http server for no real-world gain.
#
# Deliberately no GITHUB_TOKEN: if ZS_GITHUB_API is ever influenced, a
# token-sending installer leaks the user's PAT. ZS_VERSION is the rate-limit
# escape hatch instead.
fetch_to() {
	if have curl; then
		curl -fsSL --proto-redir '=https' -o "$2" "$1"
	elif have wget; then
		wget -qO "$2" "$1"
	else
		die "need curl or wget"
	fi
}

fetch_stdout() {
	if have curl; then
		curl -fsSL --proto-redir '=https' "$1"
	elif have wget; then
		wget -qO- "$1"
	else
		die "need curl or wget"
	fi
}

# ---------------------------------------------------------------- version ----

resolve_version() {
	local v body
	if [ -n "${ZS_VERSION:-}" ]; then
		v="${ZS_VERSION#v}"
	else
		body="$(fetch_stdout "$GH_API/repos/$1/releases/latest")" ||
			die "could not reach the GitHub API — it rate-limits to 60/hr per IP; pin a release with ZS_VERSION=X.Y.Z"
		# Split on commas first: the body is one long line, so a greedy .* would
		# happily match across into a later field and hand back the wrong string.
		v="$(printf '%s' "$body" | tr ',' '\n' |
			sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
		v="${v#v}"
	fi
	[ -n "$v" ] || die "could not resolve the latest version"
	# The version lands in both a URL and a filename, so a crafted tag is an
	# injection surface. Refuse anything that isn't version-shaped.
	case "$v" in
	*[!0-9A-Za-z.+_-]*) die "refusing an unexpected version string: $v" ;;
	esac
	printf '%s' "$v"
}

# --------------------------------------------------------------- checksum ----

sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif have openssl; then
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	else
		# Installing an unverified binary is not a fallback we offer.
		die "no sha256 tool found (need sha256sum, shasum, or openssl)"
	fi
}

# verify <file> <checksums.txt> <assetname>
verify() {
	local want got
	# Match on the filename column so an entry for a different asset can never
	# satisfy this one. A leading '*' marks binary mode in sha256sum output.
	want="$(awk -v f="$3" '{ n = $2; sub(/^\*/, "", n); if (n == f) print $1 }' "$2" | head -n 1)"
	[ -n "$want" ] || die "$3 is not listed in checksums.txt"
	got="$(sha256_of "$1")"
	[ "$want" = "$got" ] || die "checksum mismatch for $3 (want $want, got $got) — nothing installed"
}

# ------------------------------------------------------------ install dir ----

# state_dir mirrors Go's os.UserConfigDir(), which internal/paths.StateDir()
# builds on. The marker has to land where the binary will look for it.
state_dir() {
	case "$1" in
	darwin) printf '%s/Library/Application Support/zerosignal' "$HOME" ;;
	*) printf '%s/zerosignal' "${XDG_CONFIG_HOME:-$HOME/.config}" ;;
	esac
}

marker_file() { printf '%s/install-source-%s' "$(state_dir "$1")" "$product"; }

# canon prints a directory path with symlinks and .. resolved. readlink -f is
# GNU-only, so do it with cd/pwd -P.
canon() { (cd "$1" 2>/dev/null && pwd -P) || printf '%s' "$1"; }

# resolve_dir is deliberately STICKY. `install-service` bakes the literal
# os.Executable() path into the launchd plist / systemd unit, so an upgrade that
# lands the binary somewhere new orphans the installed service — it keeps
# starting the old path forever. Concretely: a first install falls back to
# ~/.local/bin because /usr/local/bin is root-owned; the user later installs
# Homebrew, which chowns /usr/local/bin; a non-sticky re-run would then install
# to a different directory and silently stop upgrading the binary the service
# actually runs.
resolve_dir() {
	local os="$1" recorded
	if [ -n "${ZS_INSTALL_DIR:-}" ]; then
		printf '%s' "$ZS_INSTALL_DIR"
		return
	fi
	recorded="$(sed -n 's/^bin=//p' "$(marker_file "$os")" 2>/dev/null | head -n 1)"
	if [ -n "$recorded" ] && [ -d "$(dirname "$recorded")" ] && [ -w "$(dirname "$recorded")" ]; then
		dirname "$recorded"
		return
	fi
	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		printf '/usr/local/bin'
		return
	fi
	printf '%s/.local/bin' "$HOME"
}

# guard_managed_install refuses to shadow a binary Homebrew or Scoop owns:
# upgrading it out from under the package manager leaves two copies and a
# confused `brew upgrade`. These substrings must stay in lockstep with
# internal/update/update.go::classifyPath, which reads the same evidence to
# decide what upgrade command to suggest — install_test.go pins that.
guard_managed_install() {
	local existing lower
	existing="$(command -v "$product" 2>/dev/null || true)"
	[ -n "$existing" ] || return 0
	existing="$(canon "$(dirname "$existing")")/$product"
	lower="$(printf '%s' "$existing" | tr '[:upper:]' '[:lower:]')"
	case "$lower" in
	*/caskroom/* | */cellar/* | */homebrew/*)
		[ -n "${ZS_FORCE:-}" ] || die "Homebrew already manages $existing — run 'brew upgrade $product', or set ZS_FORCE=1 to install anyway"
		;;
	*/scoop/apps/* | */scoop/shims/*)
		[ -n "${ZS_FORCE:-}" ] || die "Scoop already manages $existing — run 'scoop update $product', or set ZS_FORCE=1 to install anyway"
		;;
	esac
}

# install_binary replaces the target via a rename within the SAME directory.
# Writing over a running binary in place fails with ETXTBSY on Linux, whereas
# rename(2) unlinks the old inode and succeeds; and a mv across the tmpfs
# boundary would not be atomic.
install_binary() {
	local src="$1" target="$2" dir tmpname
	dir="$(dirname "$target")"
	tmpname="$dir/.$product.tmp.$$"
	if [ -n "${ZS_USE_SUDO:-}" ]; then
		# sudo ONLY here. `sudo sh` would set HOME=/root, putting the marker in
		# root's config dir where the user's binary never reads it.
		sudo install -m 0755 "$src" "$target" || die "sudo install to $target failed"
		return
	fi
	[ -w "$dir" ] || die "$dir is not writable — set ZS_INSTALL_DIR, or add ZS_USE_SUDO=1"
	cp "$src" "$tmpname" || die "could not stage into $dir"
	chmod 0755 "$tmpname"
	mv -f "$tmpname" "$target" || {
		rm -f "$tmpname"
		die "could not replace $target"
	}
}

write_marker() {
	local os="$1" target="$2" version="$3" dir file
	dir="$(state_dir "$os")"
	file="$(marker_file "$os")"
	mkdir -p "$dir" 2>/dev/null || {
		warn "could not create $dir — the upgrade hint will point at the releases page"
		return 0
	}
	# Best-effort: the marker only tailors the "newer version available" hint.
	{
		printf 'source=script\n'
		printf 'bin=%s\n' "$target"
		printf 'version=%s\n' "$version"
	} >"$file" 2>/dev/null || return 0
	chmod 0600 "$file" 2>/dev/null || true
}

# ----------------------------------------------------------------- output ----

# path_advice never edits a shell rc file. Which file to edit (.zshrc,
# .bashrc, .bash_profile, .profile, config.fish), staying idempotent across
# re-runs, and ordering against the user's own PATH edits are all bugs waiting
# to happen, and the payoff is small for a start-once-then-service tool.
path_advice() {
	local dir="$1" sh rc
	case ":$PATH:" in
	*":$dir:"*) return 0 ;;
	esac
	sh="$(basename "${SHELL:-sh}")"
	case "$sh" in
	zsh) rc='.zshrc' ;;
	bash) rc='.bashrc' ;;
	fish) rc='.config/fish/config.fish' ;;
	*) rc='.profile' ;;
	esac
	printf '\n'
	warn "$dir is not on your PATH"
	if [ "$sh" = fish ]; then
		printf '  add this to ~/%s:\n\n    fish_add_path %s\n\n' "$rc" "$dir"
	else
		# shellcheck disable=SC2016
		# $PATH is printed literally — it is the line the user pastes, not a
		# value we expand here.
		printf '  add this to ~/%s:\n\n    export PATH="%s:$PATH"\n\n' "$rc" "$dir"
	fi
}

# shadow_check catches the other half of the PATH problem: the install
# succeeded but something earlier on PATH still wins. Silently installing a
# binary the user can't reach is the worst outcome here.
shadow_check() {
	local target="$1" found
	found="$(command -v "$product" 2>/dev/null || true)"
	[ -n "$found" ] || return 0
	found="$(canon "$(dirname "$found")")/$product"
	[ "$found" != "$(canon "$(dirname "$target")")/$product" ] || return 0
	warn "'$product' on your PATH still resolves to $found, not $target"
}

epilogue() {
	local target="$1" version="$2"
	printf '\n%s %s installed to %s\n\n' "$product" "$version" "$target"
	if [ "$product" = "zs-node" ]; then
		cat <<-EOF
			Next steps:
			  $target --help
			  Operator guide: https://docs.zerosignal.ai/operators/installation

			Then run it under your OS supervisor:
			  $target install-service
		EOF
		return
	fi
	# Deliberately does NOT start the proxy. First run with no wallet has to ask
	# on the terminal how to set one up, and a piped installer has no terminal.
	cat <<-EOF
		Next steps (create and fund your account at https://zerosignal.ai first):
		  $target wallet login          # sign in with your zerosignal.ai passkey
		  $target proxy start           # start on :9376 (run this in a terminal)
		  $target connect               # point your AI tools at it

		Optional — start at login and restart on failure:
		  $target proxy install-service
	EOF
}

# ------------------------------------------------------------------- main ----

main() {
	local os arch repo version asset base dir target

	product="$(resolve_product "${1:-}")"
	repo="${ZS_REPO:-txnlab/$product}"
	os="$(detect_os)"
	arch="$(detect_arch)"

	guard_managed_install

	version="$(resolve_version "$repo")"
	asset="${product}_${version}_${os}_$(asset_arch "$os" "$arch").tar.gz"
	base="$GH/$repo/releases/download/v$version"

	tmpdir="$(mktemp -d 2>/dev/null || mktemp -d -t "$product")" || die "could not create a temp dir"
	trap cleanup EXIT INT TERM HUP

	info "downloading $asset"
	fetch_to "$base/$asset" "$tmpdir/$asset" || die "download failed: $base/$asset"
	fetch_to "$base/checksums.txt" "$tmpdir/checksums.txt" || die "could not fetch checksums.txt from $base"

	# Verify BEFORE extracting — never unpack unverified bytes.
	verify "$tmpdir/$asset" "$tmpdir/checksums.txt" "$asset"

	# Extract only the member we want, so a hostile archive can't scatter files.
	tar -xzf "$tmpdir/$asset" -C "$tmpdir" "$product" || die "could not extract $product from $asset"

	dir="$(resolve_dir "$os")"
	mkdir -p "$dir" || die "could not create $dir"
	target="$(canon "$dir")/$product"

	install_binary "$tmpdir/$product" "$target"
	write_marker "$os" "$target" "$version"

	# Proof the binary actually runs. A failure here is worth surfacing but is
	# not fatal — the install itself succeeded.
	"$target" version >/dev/null 2>&1 || warn "$target did not run cleanly"

	path_advice "$dir"
	shadow_check "$target"
	epilogue "$target" "$version"
}

main "$@"
