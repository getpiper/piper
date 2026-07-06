#!/bin/sh
# Piper installer — https://github.com/getpiper/piper
# Installs the piperd agent (+ systemd unit) or just the piper CLI.
set -eu

PIPER_REPO="${PIPER_REPO:-getpiper/piper}"
PIPER_BASE_URL="${PIPER_BASE_URL:-https://github.com}"
PIPER_API_URL="${PIPER_API_URL:-https://api.github.com}"
PIPER_VERSION="${PIPER_VERSION:-}"
PIPER_PREFIX="${PIPER_PREFIX:-}"
PIPER_SYSTEMD_DIR="${PIPER_SYSTEMD_DIR:-/etc/systemd/system}"
PIPER_ENV_DIR="${PIPER_ENV_DIR:-/etc/piper}"
cli_only="${PIPER_CLI_ONLY:-}"
use_rc="${PIPER_RC:-}"
no_enable=""

die() { echo "piper-install: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
	case "$1" in
		--cli-only) cli_only=1 ;;
		--rc) use_rc=1 ;;
		--no-enable) no_enable=1 ;;
		--version) shift; PIPER_VERSION="${1:-}" ;;
		--version=*) PIPER_VERSION="${1#--version=}" ;;
		-h|--help) echo "Usage: install.sh [--cli-only] [--rc] [--version vX.Y.Z]"; exit 0 ;;
		*) die "unknown option: $1" ;;
	esac
	shift
done

detect_os() {
	os="$(uname -s)"
	case "$os" in
		Linux) echo linux ;;
		Darwin) echo darwin ;;
		*) die "unsupported OS: $os" ;;
	esac
}

detect_arch() {
	arch="$(uname -m)"
	case "$arch" in
		x86_64|amd64) echo amd64 ;;
		aarch64|arm64) echo arm64 ;;
		armv7l|armv7) echo armv7 ;;
		*) die "unsupported architecture: $arch" ;;
	esac
}

fetch() { # fetch URL DEST
	if have curl; then curl -fsSL "$1" -o "$2"
	elif have wget; then wget -qO "$2" "$1"
	else die "need curl or wget"; fi
}

fetch_stdout() { # fetch URL -> stdout
	if have curl; then curl -fsSL "$1"
	elif have wget; then wget -qO- "$1"
	else die "need curl or wget"; fi
}

sha256_of() { # sha256_of FILE -> hash
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'
	elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
	else die "need sha256sum or shasum"; fi
}

# first_tag reads a GitHub releases JSON body on stdin and echoes the first
# tag_name. grep -o isolates each match (robust to pretty or compact JSON);
# head -n1 takes the newest (GitHub lists newest first).
first_tag() {
	grep -o '"tag_name": *"[^"]*"' | head -n1 | sed -E 's/.*"([^"]+)"$/\1/'
}

# resolve_version echoes the release tag to install.
resolve_version() {
	[ -n "$PIPER_VERSION" ] && { echo "$PIPER_VERSION"; return; }
	if [ -n "$use_rc" ]; then
		tag="$(fetch_stdout "$PIPER_API_URL/repos/$PIPER_REPO/releases" | first_tag)" || true
		[ -n "${tag:-}" ] || die "could not resolve latest pre-release from GitHub"
		echo "$tag"
	else
		tag="$(fetch_stdout "$PIPER_API_URL/repos/$PIPER_REPO/releases/latest" | first_tag)" || true
		[ -n "${tag:-}" ] || die "no stable release yet — re-run with --rc to install the latest pre-release"
		echo "$tag"
	fi
}

# download_verify NAME TAG OS ARCH DESTDIR
download_verify() {
	name="$1"; tag="$2"; os="$3"; arch="$4"; dest="$5"
	ver="${tag#v}"
	archive="${name}_${ver}_${os}_${arch}.tar.gz"
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/$archive" "$tmp/$archive" \
		|| die "download failed: $archive"
	fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/checksums.txt" "$tmp/checksums.txt" \
		|| die "download failed: checksums.txt"
	want="$(grep " ${archive}\$" "$tmp/checksums.txt" | awk '{print $1}')"
	[ -n "$want" ] || die "no checksum for $archive"
	got="$(sha256_of "$tmp/$archive")"
	[ "$want" = "$got" ] || die "checksum mismatch for $archive (want $want got $got)"
	tar xzf "$tmp/$archive" -C "$tmp"
	install -m 0755 "$tmp/$name" "$dest/$name"
	rm -rf "$tmp"
	trap - EXIT
}

cli_prefix() {
	[ -n "$PIPER_PREFIX" ] && { echo "$PIPER_PREFIX"; return; }
	if [ "$(id -u)" -eq 0 ]; then echo /usr/local/bin; else echo "$HOME/.local/bin"; fi
}

install_cli() { # install_cli OS ARCH TAG
	prefix="$(cli_prefix)"
	mkdir -p "$prefix"
	download_verify piper "$3" "$1" "$2" "$prefix"
	echo "installed piper $3 -> $prefix/piper"
	case ":$PATH:" in
		*":$prefix:"*) ;;
		*) echo "note: $prefix is not on your PATH — add it to use piper" ;;
	esac
}

install_agent() { # install_agent OS ARCH TAG
	os="$1"; arch="$2"; tag="$3"
	[ "$os" = linux ] || die "the full agent install needs Linux + systemd; on macOS use --cli-only (launchd support tracked in #56)"
	prefix="${PIPER_PREFIX:-/usr/local/bin}"
	if [ -z "$PIPER_PREFIX" ] && [ "$(id -u)" -ne 0 ]; then
		die "the full agent install needs root — re-run with sudo, or use --cli-only"
	fi
	mkdir -p "$prefix"
	download_verify piperd "$tag" "$os" "$arch" "$prefix"
	download_verify piper "$tag" "$os" "$arch" "$prefix"

	mkdir -p "$PIPER_SYSTEMD_DIR"
	fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/piperd.service" \
		"$PIPER_SYSTEMD_DIR/piperd.service" || die "download failed: piperd.service"

	mkdir -p "$PIPER_ENV_DIR"
	chmod 0700 "$PIPER_ENV_DIR" 2>/dev/null || true
	if [ ! -f "$PIPER_ENV_DIR/piperd.env" ]; then
		fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/piperd.env.example" \
			"$PIPER_ENV_DIR/piperd.env" || die "download failed: piperd.env.example"
		chmod 0600 "$PIPER_ENV_DIR/piperd.env" 2>/dev/null || true
	fi
	echo "installed piperd + piper $tag -> $prefix"

	if [ -z "$no_enable" ] && have systemctl; then
		systemctl daemon-reload
		systemctl enable --now piperd
		echo "piperd service enabled and started"
	else
		echo "note: service not enabled (no systemctl or --no-enable); start with: systemctl enable --now piperd"
	fi
}

os="$(detect_os)"
arch="$(detect_arch)"
tag="$(resolve_version)"
if [ -n "$cli_only" ]; then
	install_cli "$os" "$arch" "$tag"
else
	install_agent "$os" "$arch" "$tag"
fi
