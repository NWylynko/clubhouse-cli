#!/usr/bin/env sh
set -eu

BIN="club"
REPO="${CLUB_REPO:-nwylynko/clubhouse-cli}"
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
chmod +x "$tmp/$ASSET"
mv "$tmp/$ASSET" "$DEST/$BIN.tmp.$$"
mv "$DEST/$BIN.tmp.$$" "$DEST/$BIN"

echo "club: installed $DEST/$BIN ($("$DEST/$BIN" --version 2>/dev/null || echo 'not runnable on this machine'))"
case ":$PATH:" in
	*":$DEST:"*) ;;
	*)
		line="export PATH=\"$DEST:\$PATH\""
		case "${SHELL##*/}" in
			zsh) rc="${ZDOTDIR:-$HOME}/.zshrc" ;;
			bash) if [ "$os" = darwin ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi ;;
			fish) rc="$HOME/.config/fish/config.fish"; line="fish_add_path $DEST" ;;
			*) rc="$HOME/.profile" ;;
		esac
		if ! grep -qsF "$line" "$rc"; then
			mkdir -p "$(dirname "$rc")"
			printf '\n# added by the club installer\n%s\n' "$line" >> "$rc"
			echo "club: added $DEST to PATH in $rc"
		fi
		echo "club: open a new terminal (or run: $line) to use club"
		;;
esac
if [ -n "$PIN" ]; then
	if ! (: </dev/tty) 2>/dev/null; then
		echo "club: installed, but there is no terminal to join from — run: $BIN join $PIN" >&2
		exit 1
	fi
	echo "club: joining $PIN"
	exec "$DEST/$BIN" join "$PIN" </dev/tty
fi
