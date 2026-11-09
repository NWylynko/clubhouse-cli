//go:build !((linux || darwin) && (amd64 || arm64))

package git

// No bundled git is built for this GOOS/GOARCH, so Bin() fails loudly here.
// Released platforms are linux and darwin on amd64/arm64 — all embed one.
var embeddedArchive []byte
