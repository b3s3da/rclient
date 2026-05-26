#!/bin/sh
# rclient-agent — one-shot installer for boxes you want to manage.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/USER/rclient/main/install-agent.sh | sudo sh
#
# Or with the connect blob inline:
#   curl -fsSL https://raw.githubusercontent.com/USER/rclient/main/install-agent.sh \
#     | sudo sh -s -- --connect eyJ1...
#
# What it does:
#   1. Detects your CPU arch (amd64 / arm64).
#   2. Downloads the matching rclient-agent binary from the latest GitHub release.
#   3. Verifies sha256 against the SHA256SUMS file in the same release.
#   4. Runs `rclient-agent install`, which prompts for the connect token and
#      sets up systemd or OpenRC.
#
# Flags:
#   --connect BLOB     paste the connect string from the panel; otherwise
#                      `install` will prompt for it interactively
#   --version vX.Y.Z   pin a specific release (default: latest)
#   --repo USER/NAME   override the GitHub repo (default: built-in)
set -eu

REPO="${RCLIENT_REPO:-b3s3da/rclient}"
VERSION=""
CONNECT=""

while [ $# -gt 0 ]; do
	case "$1" in
		--connect) CONNECT="$2"; shift 2 ;;
		--version) VERSION="$2"; shift 2 ;;
		--repo)    REPO="$2";    shift 2 ;;
		-h|--help)
			grep '^#' "$0" | sed 's/^# \{0,1\}//'
			exit 0 ;;
		*) echo "unknown arg: $1" >&2; exit 2 ;;
	esac
done

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

[ "$(id -u)" -eq 0 ] || die "run me as root: curl ... | sudo sh"

# --- arch detect ---
case "$(uname -m)" in
	x86_64|amd64)  ARCH=amd64 ;;
	aarch64|arm64) ARCH=arm64 ;;
	*) die "unsupported arch: $(uname -m)" ;;
esac
ASSET="rclient-agent-linux-$ARCH"

# --- pick a downloader ---
if   have curl; then DL='curl -fsSL -o';
elif have wget; then DL='wget -qO';
else die "need curl or wget"
fi

# --- resolve version ---
if [ -z "$VERSION" ]; then
	BASE="https://github.com/$REPO/releases/latest/download"
else
	BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> downloading $ASSET ($BASE)"
$DL "$TMP/$ASSET"        "$BASE/$ASSET"
$DL "$TMP/SHA256SUMS"    "$BASE/SHA256SUMS"

# --- verify ---
if have sha256sum; then
	(cd "$TMP" && grep " $ASSET\$" SHA256SUMS | sha256sum -c -) \
		|| die "checksum mismatch — refusing to install"
elif have shasum; then
	(cd "$TMP" && grep " $ASSET\$" SHA256SUMS | shasum -a 256 -c -) \
		|| die "checksum mismatch — refusing to install"
else
	echo "warning: no sha256sum/shasum tool found, skipping verification" >&2
fi

chmod +x "$TMP/$ASSET"

# --- run install (interactive unless --connect given) ---
if [ -n "$CONNECT" ]; then
	"$TMP/$ASSET" install --connect "$CONNECT"
else
	# Make sure the prompt can read from the user even when stdin is a pipe
	# (curl|sh) — install_unix.go reads /dev/tty for the secret.
	"$TMP/$ASSET" install
fi
