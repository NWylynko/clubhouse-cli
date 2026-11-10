package sessionnet

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nwylynko/clubhouse-cli/clientaddr"
)

const (
	RelayPath  = "/relay/v1"
	HealthPath = "/healthz"
)

func NormalizeRelayURL(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("sessionnet: no relay address")
	}
	if !strings.Contains(addr, "://") {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return "", fmt.Errorf("sessionnet: relay address %q is neither a URL nor host:port: %w", addr, err)
		}
		addr = "ws://" + addr + RelayPath
	}
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("sessionnet: relay address %q: %w", addr, err)
	}
	switch u.Scheme {
	case "ws", "wss":
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("sessionnet: relay address %q must be ws:// or wss:// (or a bare host:port)", addr)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = RelayPath
	}
	if u.Scheme == "ws" && !isPrivateHost(u.Hostname()) {
		return "", fmt.Errorf(
			"sessionnet: relay %s is plain ws:// — the session PIN rides the transport in the clear on every register and connect. Use wss://%s%s",
			u.String(), u.Host, u.Path)
	}
	return u.String(), nil
}

func RelayDialURL(addr, pin, service string) (string, error) {
	base, err := NormalizeRelayURL(addr)
	if err != nil {
		return "", err
	}
	if pin == "" || service == "" {
		return "", fmt.Errorf("sessionnet: a relay dial must name a pin and a service")
	}
	return strings.TrimSuffix(base, "/") + "/" + url.PathEscape(pin) + "/" + url.PathEscape(service), nil
}

func isPrivateHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func dialRelay(ctx context.Context, addr, pin, service string, dial dialFunc) (net.Conn, error) {
	target, err := RelayDialURL(addr, pin, service)
	if err != nil {
		return nil, err
	}
	opts := &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: relayTransport(dial)},
		CompressionMode: websocket.CompressionDisabled,
	}
	c, _, err := websocket.Dial(ctx, target, opts)
	if err != nil {
		return nil, fmt.Errorf("sessionnet: dial relay %s: %w", target, err)
	}
	return newRelayConn(websocket.NetConn(context.WithoutCancel(ctx), c, websocket.MessageBinary), c, ""), nil
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func relayTransport(dial dialFunc) http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if dial != nil {
		t.DialContext = dial
	}
	return t
}

type relayConn struct {
	net.Conn
	ws     *websocket.Conn
	source string

	readq   chan []byte
	pumped  chan struct{}
	pumpMu  sync.Mutex
	pumpErr error
	closed  chan struct{}
	once    sync.Once

	rest []byte

	dlMu    sync.Mutex
	readDL  time.Time
	dlReset chan struct{}
}

func newRelayConn(inner net.Conn, ws *websocket.Conn, source string) *relayConn {
	c := &relayConn{
		Conn: inner, ws: ws, source: source,
		readq:   make(chan []byte),
		pumped:  make(chan struct{}),
		closed:  make(chan struct{}),
		dlReset: make(chan struct{}),
	}
	go c.pump()
	return c
}

func (c *relayConn) pump() {
	defer close(c.pumped)
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Conn.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case c.readq <- chunk:
			case <-c.closed:
				return
			}
		}
		if err != nil {
			c.pumpMu.Lock()
			c.pumpErr = err
			c.pumpMu.Unlock()
			return
		}
	}
}

func (c *relayConn) Read(p []byte) (int, error) {
	for {
		if len(c.rest) > 0 {
			n := copy(p, c.rest)
			c.rest = c.rest[n:]
			return n, nil
		}
		c.dlMu.Lock()
		dl, reset := c.readDL, c.dlReset
		c.dlMu.Unlock()
		var expired <-chan time.Time
		var timer *time.Timer
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			expired = timer.C
		}
		stop := func() {
			if timer != nil {
				timer.Stop()
			}
		}
		select {
		case chunk := <-c.readq:
			stop()
			c.rest = chunk
		case <-c.pumped:
			stop()
			select {
			case chunk := <-c.readq:
				c.rest = chunk
				continue
			default:
			}
			c.pumpMu.Lock()
			err := c.pumpErr
			c.pumpMu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return 0, err
		case <-expired:
			return 0, os.ErrDeadlineExceeded
		case <-reset:
			stop()
		}
	}
}

func (c *relayConn) SetReadDeadline(t time.Time) error {
	c.dlMu.Lock()
	c.readDL = t
	close(c.dlReset)
	c.dlReset = make(chan struct{})
	c.dlMu.Unlock()
	return nil
}

func (c *relayConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *relayConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *relayConn) Ping(ctx context.Context) error { return c.ws.Ping(ctx) }

func (c *relayConn) ClientSource() string { return c.source }

type pinger interface {
	Ping(context.Context) error
}

func acceptRelayConn(w http.ResponseWriter, r *http.Request, hops int) (net.Conn, error) {
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		http.Error(w, "browser origins are refused", http.StatusForbidden)
		return nil, fmt.Errorf("sessionnet: browser origin refused")
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	src := clientaddr.FromRequest(r, hops)
	return newRelayConn(websocket.NetConn(context.Background(), c, websocket.MessageBinary), c, src), nil
}

func relayHTTPHandler(rel *Relay, onConn func(net.Conn)) http.Handler {
	hops := 0
	if rel != nil {
		hops = rel.ProxyHops
	}
	mux := http.NewServeMux()
	mux.HandleFunc(RelayPath+"/{pin}/{service}", func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptRelayConn(w, r, hops)
		if err != nil {
			return
		}
		go onConn(c)
	})
	mux.HandleFunc(RelayPath, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "a relay dial must name the session: "+RelayPath+"/<pin>/<service>\n", http.StatusNotFound)
	})
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if rel != nil && !rel.allowHealth() {
			http.Error(w, "rate-limited\n", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
