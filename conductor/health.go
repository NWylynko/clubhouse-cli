package conductor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/controlplane"
)

func HTTPHealth(url string) func(ctx context.Context) error {
	return HTTPHealthDynamic(func() string { return url })
}

func HTTPHealthDynamic(urlFor func() string) func(ctx context.Context) error {
	client := &http.Client{Timeout: 5 * time.Second}
	return func(ctx context.Context) error {
		url := ""
		if urlFor != nil {
			url = urlFor()
		}
		if url == "" {
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("dev server unreachable at %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			return fmt.Errorf("dev server answered %s with %s", url, resp.Status)
		}
		return nil
	}
}

func SessionDevURL(ctx context.Context, client *controlplane.Client, pin string, host *canonical.Host) string {
	if client == nil {
		return ""
	}
	url, err := client.EndpointURL(ctx, pin, controlplane.EndpointDev)
	if err != nil {
		return ""
	}
	return url
}
