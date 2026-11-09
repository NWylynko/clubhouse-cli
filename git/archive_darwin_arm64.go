//go:build darwin && arm64

package git

import _ "embed"

//go:embed bundled/darwin-arm64.tar.xz
var embeddedArchive []byte
