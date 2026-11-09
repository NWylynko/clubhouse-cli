#!/usr/bin/env bash
# Download the pinned static git distribution into git/bundled/ so
# //go:embed can bake it into the club binary. Pin + checksums live here —
# bump Version in git/version.go in lockstep.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/git/bundled"
VERSION="2.52.0"
LINUX="https://github.com/baulk/git-minimal/releases/download/v${VERSION}"
# baulk/git-minimal has no darwin build, so we build our own:
# .github/workflows/bundled-git-darwin.yml publishes this prerelease.
DARWIN="https://github.com/nwylynko/clubhouse-cli/releases/download/bundled-git-v${VERSION}"

# goos-goarch → asset URL + sha256
declare -A ASSETS=(
  ["linux-amd64"]="${LINUX}/git-minimal-musl-v${VERSION}-linux-amd64.tar.xz"
  ["linux-arm64"]="${LINUX}/git-minimal-musl-v${VERSION}-linux-arm64.tar.xz"
  ["darwin-amd64"]="${DARWIN}/git-darwin-amd64-v${VERSION}.tar.xz"
  ["darwin-arm64"]="${DARWIN}/git-darwin-arm64-v${VERSION}.tar.xz"
)
declare -A SUMS=(
  ["linux-amd64"]="d2edd4a60cc80ea05cedaeafef6a8f680e77b4fbcfb4ec03035f0527520576e0"
  ["linux-arm64"]="4258e28fb48486b99ad57a64a659d9e08633ac758615cd17dd6ecb0083f051bf"
  ["darwin-amd64"]="c9855aa5009c55b05e9e90fd83a91ed06b98be76a57eeaae397a42d79018f68d"
  ["darwin-arm64"]="cf30b06b12eff0659c235fb83155251771d397156ef90cbc67d990134df1390b"
)

mkdir -p "$OUT"

fetch_one() {
  local key="$1" url dest sum
  url="${ASSETS[$key]}"
  dest="${OUT}/${key}.tar.xz"
  sum="${SUMS[$key]}"

  # An unpinned asset would be baked, unverified, into a published box image
  # (plan 23) — refuse rather than warn.
  if [[ -z "$sum" ]]; then
    echo "error: no pinned checksum for $key — add its sha256 above before fetching" >&2
    exit 1
  fi

  if [[ -f "$dest" ]]; then
    echo "$sum  $dest" | sha256sum -c - >/dev/null
    echo "ok  $key (cached)"
    return
  fi

  echo "fetch $key ← $url"
  curl -fsSL -o "$dest.tmp" "$url"
  mv "$dest.tmp" "$dest"
  echo "$sum  $dest" | sha256sum -c -
}

# Default: host platform. Pass "all" to fetch every known asset.
TARGET="${1:-$(go env GOOS)-$(go env GOARCH)}"
if [[ "$TARGET" == "all" ]]; then
  for k in "${!ASSETS[@]}"; do fetch_one "$k"; done
elif [[ -n "${ASSETS[$TARGET]+x}" ]]; then
  fetch_one "$TARGET"
else
  echo "error: no bundled git asset for $TARGET" >&2
  exit 1
fi
