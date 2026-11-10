package box

import (
	"context"
	"fmt"
	"net"
	"strings"
)

func AdvertiseIP(ctx context.Context, r Runner) (string, error) {
	if out, err := r.Run(ctx, "tailscale ip -4"); err == nil {
		if ip := firstRoutableIPv4(out); ip != "" {
			return ip, nil
		}
	}
	out, err := r.Run(ctx, "hostname -I")
	if ip := firstRoutableIPv4(out); ip != "" {
		return ip, nil
	}
	said := strings.TrimSpace(firstLine(out))
	switch {
	case said != "":
		return "", fmt.Errorf("could not determine %s's routable address: `hostname -I` said %q",
			r.Target(), said)
	case err != nil:
		return "", fmt.Errorf("could not determine %s's routable address: `hostname -I`: %w",
			r.Target(), err)
	default:
		return "", fmt.Errorf("could not determine %s's routable address: `hostname -I` listed none "+
			"(no Tailscale address either)", r.Target())
	}
}

func firstRoutableIPv4(out string) string {
	for _, f := range strings.Fields(out) {
		ip := net.ParseIP(strings.SplitN(f, "%", 2)[0])
		if ip == nil || ip.To4() == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			continue
		}
		return ip.String()
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
