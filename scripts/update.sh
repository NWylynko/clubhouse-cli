#!/usr/bin/env sh
set -eu

BIN="club"
SITE="${CLUB_SITE:-https://clubhouse.sh}"

DEST="${CLUB_INSTALL_DIR:-}"
if [ -z "$DEST" ]; then
	existing="$(command -v "$BIN" 2>/dev/null || true)"
	if [ -z "$existing" ]; then
		echo "club: there is no $BIN on your PATH to update. Install it first:" >&2
		echo "  curl -fsSL $SITE/install.sh | sh" >&2
		exit 1
	fi
	DEST="$(CDPATH= cd -- "$(dirname -- "$existing")" && pwd)"
fi

if ! command -v curl >/dev/null 2>&1; then
	echo "club: curl is needed to fetch the installer from $SITE" >&2
	exit 1
fi

echo "club: updating the $BIN in $DEST"
curl -fsSL "$SITE/install.sh" | CLUB_INSTALL_DIR="$DEST" sh
