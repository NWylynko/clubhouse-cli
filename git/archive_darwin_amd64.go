//go:build darwin && amd64

package git

import _ "embed"

//go:embed bundled/darwin-amd64.tar.xz
var embeddedArchive []byte
