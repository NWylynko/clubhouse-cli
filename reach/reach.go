package reach

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/sessionnet"
)

type SessionService string

const (
	ServiceGit SessionService = controlplane.EndpointGit
	ServiceDev SessionService = controlplane.EndpointDev
)

type Result struct {
	Published bool
	Reachable bool
	Via       string
}

func ProbeSessionService(ctx context.Context, client *controlplane.Client, sess controlplane.Session, pin string, svc SessionService) Result {
	if client == nil {
		return Result{}
	}
	switch svc {
	case ServiceGit:
		// raw endpoint ok: Dialable resolves before probe; published URL is not dialed.
		ep, ok := sess.Endpoints[controlplane.EndpointGit]
		if !ok || ep.URL == "" {
			return Result{}
		}
		dialable, err := client.Dialable(ctx, sess, ep.URL)
		if err != nil || dialable == "" {
			return Result{}
		}
		return Result{
			Published: true,
			Reachable: probeHTTP(ctx, gitInfoRefs(dialable)),
			Via:       sessionnet.LastPath(pin, controlplane.EndpointGit),
		}
	case ServiceDev:
		url, err := client.EndpointURL(ctx, pin, controlplane.EndpointDev)
		if err != nil || url == "" {
			return Result{}
		}
		return Result{Published: true, Reachable: probeHTTP(ctx, url)}
	default:
		return Result{}
	}
}

func gitInfoRefs(dial string) string {
	if dial == "" {
		return ""
	}
	return strings.TrimRight(dial, "/") + "/info/refs?service=git-upload-pack"
}

func ProbeHTTP(ctx context.Context, url string) bool {
	return probeHTTP(ctx, url)
}

func probeHTTP(ctx context.Context, url string) bool {
	if url == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}
