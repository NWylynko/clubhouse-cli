#!/usr/bin/env sh
# club installer — the one install path there is.
#
# A GitHub Release is the whole distribution channel (plan 47 step 10; homebrew
# was dropped, decided not deferred). This fetches the asset for THIS machine's
# platform from the latest release and puts it on PATH:
#
#     curl -fsSL https://clubhouse.sh | sh          # install
#     curl -fsSL https://clubhouse.sh | sh -s abc123 # install, then join that session
#
# The asset name is contractual — `make release` writes dist/club-<os>-<arch>
# and the release workflow uploads those files under exactly those names, so the
# `uname` mapping below is one third of a three-way agreement (the other two are
# the Makefile's PLATFORMS and .github/workflows/release-cli.yml, both now in
# THIS repo). Guarded by scripts/installer_test.go.
#
# REPO is this module's own repo, and the release it resolves is the one the
# workflow beside this script publishes. That is the point of plan 49 phase B:
# the script, the matrix that writes the assets, and the release that holds them
# are one checkout, so there is no version of "latest" that means two things.
#
# The repo is PUBLIC (plan 49's flip, 2026-08-15), so the download is an
# anonymous curl of the release asset URL and nothing else: no `gh`, no token,
# no API call — a one-line installer that needs a credential is not one-line.
# (Until the flip it was authenticated-only, because an anonymous path against a
# private repo could only produce a 404 that read like the release was missing.)
# Guarded by scripts/installer_test.go, which runs it with no gh and no token.
set -eu

BIN="club"
REPO="${CLUB_REPO:-nwylynko/clubhouse-cli}"
# Which release to install. Empty means the latest one.
TAG="${CLUB_VERSION:-}"
DEST="${CLUB_INSTALL_DIR:-$HOME/.local/bin}"
PIN="${1:-}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	arm64 | aarch64) arch="arm64" ;;
	*) echo "club: no release is built for this architecture: $arch (released: amd64, arm64)" >&2; exit 1 ;;
esac
case "$os" in
	darwin | linux) ;;
	*) echo "club: no release is built for this OS: $os (released: darwin, linux)" >&2; exit 1 ;;
esac

ASSET="$BIN-$os-$arch"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if ! command -v curl >/dev/null 2>&1; then
	echo "club: curl is needed to download the release" >&2
	exit 1
fi
# `releases/latest/download/<asset>` resolves the newest release server-side;
# `releases/download/<tag>/<asset>` pins one. Both redirect to the asset store,
# hence -L.
if [ -n "$TAG" ]; then
	url="https://github.com/$REPO/releases/download/$TAG/$ASSET"
else
	url="https://github.com/$REPO/releases/latest/download/$ASSET"
fi
echo "club: fetching $ASSET from $REPO ${TAG:+$TAG }release"
if ! curl -fsSL -o "$tmp/$ASSET" "$url"; then
	echo "club: download failed: $url" >&2
	echo "club: is the release published, and does it carry $ASSET? https://github.com/$REPO/releases" >&2
	exit 1
fi

if [ ! -f "$tmp/$ASSET" ]; then
	echo "club: the release had no asset named $ASSET." >&2
	exit 1
fi

mkdir -p "$DEST"
# Install to a temp name and move: replacing the bytes of a running (or
# previously run) binary in place is what gets a darwin build SIGKILLed by AMFI,
# and a partial write can never clobber a working install this way.
chmod +x "$tmp/$ASSET"
mv "$tmp/$ASSET" "$DEST/$BIN.tmp.$$"
mv "$DEST/$BIN.tmp.$$" "$DEST/$BIN"

echo "club: installed $DEST/$BIN ($("$DEST/$BIN" --version 2>/dev/null || echo 'not runnable on this machine'))"
case ":$PATH:" in
	*":$DEST:"*) ;;
	*) echo "club: $DEST is not on your PATH — add it: export PATH=\"$DEST:\$PATH\"" ;;
esac
# A PIN means "install, then be in the session". Under `curl | sh` stdin is the
# pipe the script arrived on, so the join is handed the terminal instead — on a
# pipe it would come up as the plain line log rather than the console. No
# terminal means nobody to sit in the session: say so, and what to run.
if [ -n "$PIN" ]; then
	if ! (: </dev/tty) 2>/dev/null; then
		echo "club: installed, but there is no terminal to join from — run: $BIN join $PIN" >&2
		exit 1
	fi
	echo "club: joining $PIN"
	exec "$DEST/$BIN" join "$PIN" </dev/tty
fi
