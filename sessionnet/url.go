package sessionnet

import (
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const Scheme = "club"

func FormatURL(pin, service, path string) string {
	u := Scheme + "://" + pin + "/" + service
	if p := strings.TrimPrefix(path, "/"); p != "" {
		u += "/" + p
	}
	return u
}

func IsSessionURL(raw string) bool {
	return strings.HasPrefix(raw, Scheme+"://")
}

func ParseURL(raw string) (pin, service, path string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", err
	}
	if u.Scheme != Scheme {
		return "", "", "", fmt.Errorf("sessionnet: %q is not a %s:// address", raw, Scheme)
	}
	pin = u.Host
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
	if pin == "" || parts[0] == "" {
		return "", "", "", fmt.Errorf("sessionnet: %q names no session service", raw)
	}
	service = parts[0]
	if len(parts) == 2 {
		path = parts[1]
	}
	return pin, service, path, nil
}

var (
	fwdMu    sync.Mutex
	fwdCache = map[string]*Forwarder{}
)

func LocalURL(ctx context.Context, raw, relay string, key Key, direct func(service string) string, ticket func(service string) (string, error)) (string, error) {
	pin, service, path, err := ParseURL(raw)
	if err != nil {
		return "", err
	}
	if key.Zero() {
		return "", fmt.Errorf("sessionnet: %s needs the session key — resolve the PIN through the control plane", raw)
	}
	cacheKey := pin + "/" + service
	fwdMu.Lock()
	f, ok := fwdCache[cacheKey]
	fwdMu.Unlock()
	if !ok {
		nf, err := forwardFor(context.WithoutCancel(ctx), pin, service, relay, key, direct, ticket)
		if err != nil {
			return "", err
		}
		fwdMu.Lock()
		if existing, raced := fwdCache[cacheKey]; raced {
			fwdMu.Unlock()
			_ = nf.Close()
			f = existing
		} else {
			fwdCache[cacheKey] = nf
			fwdMu.Unlock()
			f = nf
		}
	}
	out := "http://" + f.LocalHost()
	if path != "" {
		out += "/" + path
	}
	return out, nil
}

const localDomain = "club.localhost"

func DevLocalHost(pin string) string { return pin + "." + localDomain }

const devPortAttempts = 8

func devLocalPort(pin string, attempt int) int {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "devlocal:%s:%d", pin, attempt)
	return 20000 + int(h.Sum32()%40000)
}

func forwardFor(ctx context.Context, pin, service, relay string, key Key, direct func(string) string, ticket func(string) (string, error)) (*Forwarder, error) {
	cfg := ForwarderConfig{Relay: relay, PIN: pin, Service: service, Key: key, Direct: direct, Ticket: ticket}
	if service != "dev" {
		return Forward(ctx, cfg)
	}
	var err error
	for attempt := 0; attempt < devPortAttempts; attempt++ {
		port := devLocalPort(pin, attempt)
		cfg.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		cfg.AlsoAddr = []string{net.JoinHostPort("::1", strconv.Itoa(port))}
		cfg.LocalHost = net.JoinHostPort(DevLocalHost(pin), strconv.Itoa(port))
		var f *Forwarder
		if f, err = Forward(ctx, cfg); err == nil {
			return f, nil
		}
	}
	return nil, fmt.Errorf("sessionnet: no free local port for %s's dev server: %w", pin, err)
}

func CloseForwarders() {
	fwdMu.Lock()
	all := fwdCache
	fwdCache = map[string]*Forwarder{}
	fwdMu.Unlock()
	for _, f := range all {
		_ = f.Close()
	}
}
