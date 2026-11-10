//go:build !((linux || darwin) && (amd64 || arm64))

package git

var embeddedArchive []byte
