package sessionnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/logx"
)

type Dialer struct {
	Relay  string
	PIN    string
	Key    Key
	Direct func(service string) string

	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	Ticket func(service string) (string, error)
}

var lastPath sync.Map

func LastPath(pin, service string) string {
	v, ok := lastPath.Load(routeKey(pin, service))
	if !ok {
		return ""
	}
	return v.(string)
}

func recordPath(pin, service, how, addr string) {
	lastPath.Store(routeKey(pin, service), how+" "+addr)
	lastForwardErr.Delete(routeKey(pin, service))
	logx.New("sessionnet").Infof("%s: %s connected via %s %s", pin, service, how, addr)
}

var lastForwardErr sync.Map

func LastForwardError(pin, service string) error {
	v, ok := lastForwardErr.Load(routeKey(pin, service))
	if !ok {
		return nil
	}
	return v.(error)
}

func (d *Dialer) Dial(ctx context.Context, service string) (net.Conn, error) {
	if d.Key.Zero() {
		return nil, fmt.Errorf("sessionnet: no session key for %s — resolve the PIN through the control plane first", d.PIN)
	}
	if d.Direct != nil {
		if addr := d.Direct(service); addr != "" {
			dctx, cancel := context.WithTimeout(ctx, directBudget)
			c, err := d.dialDirect(dctx, service, addr)
			cancel()
			if err == nil {
				return c, nil
			} else if errors.Is(err, errHandshake) {
				return nil, err
			}
			logx.New("sessionnet").Debugf("%s: %s direct %s did not answer (%v) — using the relay", d.PIN, service, addr, err)
		}
	}
	if d.Relay == "" {
		return nil, fmt.Errorf("sessionnet: no relay address for session %s", d.PIN)
	}
	c, err := dialRelay(ctx, d.Relay, d.PIN, service, d.DialContext)
	if err != nil {
		return nil, err
	}
	ticket := ""
	if d.Ticket != nil {
		ticket, err = d.Ticket(service)
		if err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("sessionnet: relay ticket for %s: %w", service, err)
		}
	}
	line := fmt.Sprintf("%s connect %s %s", relayProto, d.PIN, service)
	if ticket != "" {
		line += " " + ticket
	}
	if _, err := fmt.Fprintf(c, "%s\n", line); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	replyLine, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sessionnet: relay did not answer: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if strings.TrimSpace(replyLine) != "ok" {
		reply := strings.TrimSpace(replyLine)
		_ = c.Close()
		switch reply {
		case "rate-limited":
			return nil, fmt.Errorf("sessionnet: relay rate-limited the %s connect", service)
		case "full":
			return nil, fmt.Errorf("sessionnet: relay is at capacity for session %s", d.PIN)
		case "unauthorized":
			return nil, fmt.Errorf("sessionnet: relay refused the %s connect — ticket missing or invalid", service)
		default:
			return nil, fmt.Errorf("sessionnet: %s has no live %s holder — nobody is serving it right now", d.PIN, service)
		}
	}
	if br.Buffered() > 0 {
		_ = c.Close()
		return nil, errors.New("sessionnet: relay sent unexpected data before the handshake")
	}
	sec, err := handshake(c, d.Key, true)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	recordPath(d.PIN, service, "relay", d.Relay)
	return sec, nil
}

const directBudget = 5 * time.Second

func (d *Dialer) dialDirect(ctx context.Context, service, addr string) (net.Conn, error) {
	c, err := dialRelay(ctx, addr, d.PIN, service, d.DialContext)
	if err != nil {
		return nil, err
	}
	release := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer release()
	if _, err := fmt.Fprintf(c, "%s direct %s %s\n", relayProto, d.PIN, service); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(directBudget))
	line, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sessionnet: direct holder %s did not answer: %w", addr, err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if strings.TrimSpace(line) != "ok" {
		_ = c.Close()
		return nil, fmt.Errorf("sessionnet: direct holder %s refused %s", addr, service)
	}
	sec, err := handshake(&prefixedConn{Conn: c, buf: br}, d.Key, true)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	release()
	recordPath(d.PIN, service, "direct", addr)
	return sec, nil
}

type prefixedConn struct {
	net.Conn
	buf *bufio.Reader
}

func (c *prefixedConn) Read(p []byte) (int, error) { return c.buf.Read(p) }

type ForwarderConfig struct {
	Relay       string
	PIN         string
	Service     string
	Key         Key
	Direct      func(service string) string
	Addr        string
	LocalHost   string
	AlsoAddr    []string
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Ticket      func(service string) (string, error)
}

type Forwarder struct {
	ln     net.Listener
	extra  []net.Listener
	cfg    ForwarderConfig
	cancel context.CancelFunc
	once   sync.Once
}

func Forward(ctx context.Context, cfg ForwarderConfig) (*Forwarder, error) {
	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	f := &Forwarder{ln: ln, cfg: cfg, cancel: cancel}
	for _, a := range cfg.AlsoAddr {
		eln, err := net.Listen("tcp", a)
		if err != nil {
			logx.New("sessionnet").Debugf("%s: %s forwarder could not also bind %s: %v", cfg.PIN, cfg.Service, a, err)
			continue
		}
		f.extra = append(f.extra, eln)
	}
	go f.acceptOn(ctx, f.ln)
	for _, eln := range f.extra {
		go f.acceptOn(ctx, eln)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		for _, eln := range f.extra {
			_ = eln.Close()
		}
	}()
	return f, nil
}

func (f *Forwarder) URL() string { return "http://" + f.ln.Addr().String() }

func (f *Forwarder) Addr() string { return f.ln.Addr().String() }

func (f *Forwarder) LocalHost() string {
	if f.cfg.LocalHost != "" {
		return f.cfg.LocalHost
	}
	return f.Addr()
}

func (f *Forwarder) Close() error {
	f.once.Do(f.cancel)
	return nil
}

func (f *Forwarder) acceptOn(ctx context.Context, ln net.Listener) {
	d := &Dialer{
		Relay: f.cfg.Relay, PIN: f.cfg.PIN, Key: f.cfg.Key,
		Direct: f.cfg.Direct, DialContext: f.cfg.DialContext,
		Ticket: f.cfg.Ticket,
	}
	for {
		local, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer local.Close()
			remote, err := d.Dial(ctx, f.cfg.Service)
			if err != nil {
				lastForwardErr.Store(routeKey(f.cfg.PIN, f.cfg.Service), err)
				logx.New("sessionnet").Warnf("%s: %s forwarder: %v", f.cfg.PIN, f.cfg.Service, err)
				return
			}
			defer remote.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
			go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
			<-done
		}()
	}
}

func RewriteHostPort(rawURL, hostPort string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.Host = hostPort
	return u.String(), nil
}
