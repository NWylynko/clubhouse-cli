package clientaddr

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/nwylynko/clubhouse-cli/logx"
)

const EnvHops = "CLUB_PROXY_HOPS"

func HopsFromEnv() int {
	n, _ := hopsFromEnv()
	return n
}

func Configured() bool {
	_, ok := hopsFromEnv()
	return ok
}

func hopsFromEnv() (int, bool) {
	n, err := ParseHops(os.Getenv(EnvHops))
	if err != nil {
		return 0, false
	}
	return n, true
}

var ErrHopsUnset = errors.New(EnvHops + " is unset")

func ParseHops(raw string) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, ErrHopsUnset
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a whole number", EnvHops, v)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s=%q is negative; a hop count cannot be", EnvHops, v)
	}
	return n, nil
}

func PeerHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func Resolve(peer, xff string, hops int) string {
	peer = strings.TrimSpace(peer)
	if peer == "" {
		peer = "unknown"
	}
	if hops <= 0 {
		return peer
	}
	parts := splitXFF(xff)
	if len(parts) == 0 || hops > len(parts) {
		if n := shortChains.Add(1); n == 1 {
			logx.New("clientaddr").Warnf(
				"%s=%d but the X-Forwarded-For chain arriving has %d entr(ies) — "+
					"falling back to the peer socket, which buckets every client behind that proxy "+
					"together. Measure the real chain (ADR 0005) and set the right value; "+
					"until then this deployment's quota counters cannot be trusted",
				EnvHops, hops, len(parts))
		}
		return peer
	}
	return parts[len(parts)-hops]
}

func ShortChains() int64 { return shortChains.Load() }

var shortChains atomic.Int64

func FromRequest(r *http.Request, hops int) string {
	if r == nil {
		return "unknown"
	}
	return Resolve(PeerHost(r.RemoteAddr), r.Header.Get("X-Forwarded-For"), hops)
}

func Describe(hops int) string {
	if hops <= 0 {
		if !Configured() {
			return fmt.Sprintf(
				"client address = peer socket (%s is UNSET — not measured). Behind a proxy or a "+
					"Cloudflare worker this buckets every client together; measure the chain the "+
					"ADR 0005 way and set it", EnvHops)
		}
		return fmt.Sprintf("client address = peer socket (%s=0); X-Forwarded-For ignored", EnvHops)
	}
	return fmt.Sprintf("client address = %s=%d (Nth from the right of X-Forwarded-For)", EnvHops, hops)
}

func splitXFF(xff string) []string {
	var out []string
	for _, p := range strings.Split(xff, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
