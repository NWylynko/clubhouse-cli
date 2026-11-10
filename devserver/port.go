package devserver

import (
	"fmt"
	"strings"
	"sync/atomic"
)

const DefaultPort = 3000

var testLocalPort atomic.Int64

func SetTestLocalPort(port int) (restore func()) {
	prev := testLocalPort.Swap(int64(port))
	return func() { testLocalPort.Store(prev) }
}

func ResolveLocalPort() (int, string) {
	if p := testLocalPort.Load(); p > 0 {
		return int(p), "test override"
	}
	return DefaultPort, ""
}

func WithPortEnv(base []string, port int) []string {
	if port <= 0 {
		return base
	}
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		if strings.HasPrefix(e, "PORT=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, fmt.Sprintf("PORT=%d", port))
}
