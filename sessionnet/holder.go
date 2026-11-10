package sessionnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/logx"
)

type HolderConfig struct {
	Relay   string
	PIN     string
	Service string
	Key     Key

	DirectListener  net.Listener
	DirectAdvertise string

	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	Ticket func() (string, error)
}

func DirectAddr(ln net.Listener) string {
	d, ok := ln.(interface{ DirectAddr() string })
	if !ok {
		return ""
	}
	return d.DirectAddr()
}

var ErrHolderBusy = errors.New("busy — the previous holder is still registered")

func Serve(ctx context.Context, cfg HolderConfig) (net.Listener, error) {
	if cfg.Relay == "" {
		return nil, errors.New("sessionnet: no relay address")
	}
	if cfg.PIN == "" || cfg.Service == "" {
		return nil, errors.New("sessionnet: pin and service required")
	}
	if cfg.Key.Zero() {
		return nil, fmt.Errorf("sessionnet: serving %s needs the session key", cfg.Service)
	}
	ctx, cancel := context.WithCancel(ctx)
	l := &holderListener{cfg: cfg, conns: make(chan net.Conn), closed: make(chan struct{}), cancel: cancel}

	ctrl, err := l.registerOnce(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	go l.run(ctx, ctrl)
	if cfg.DirectListener != nil {
		l.serveDirect(ctx)
	}
	return l, nil
}

type holderListener struct {
	cfg    HolderConfig
	conns  chan net.Conn
	closed chan struct{}
	cancel context.CancelFunc
	once   sync.Once
	br     *bufio.Reader

	mu   sync.Mutex
	ctrl net.Conn
}

func (l *holderListener) registerOnce(ctx context.Context) (net.Conn, error) {
	c, err := dialRelay(ctx, l.cfg.Relay, l.cfg.PIN, l.cfg.Service, l.cfg.DialContext)
	if err != nil {
		return nil, err
	}
	ticket := ""
	if l.cfg.Ticket != nil {
		ticket, err = l.cfg.Ticket()
		if err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("sessionnet: relay ticket for %s: %w", l.cfg.Service, err)
		}
	}
	line := fmt.Sprintf("%s register %s %s", relayProto, l.cfg.PIN, l.cfg.Service)
	if ticket != "" {
		line += " " + ticket
	}
	if _, err := fmt.Fprintf(c, "%s\n", line); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	reply, err := br.ReadString('\n')
	if err != nil || strings.TrimSpace(reply) != "ok" {
		_ = c.Close()
		got := strings.TrimSpace(reply)
		if got == "" {
			got = fmt.Sprint(err)
		}
		if got == "busy" {
			return nil, fmt.Errorf("sessionnet: relay refused registration of %s: %w", l.cfg.Service, ErrHolderBusy)
		}
		return nil, fmt.Errorf("sessionnet: relay refused registration of %s: %s", l.cfg.Service, got)
	}
	_ = c.SetReadDeadline(time.Time{})
	l.br = br
	l.mu.Lock()
	l.ctrl = c
	l.mu.Unlock()
	return c, nil
}

func (l *holderListener) run(ctx context.Context, ctrl net.Conn) {
	br := l.br
	for {
		if err := l.pump(ctx, ctrl, br); err != nil && ctx.Err() == nil {
			logx.New("sessionnet").Debugf("%s: %s tunnel dropped (%v) — reconnecting", l.cfg.PIN, l.cfg.Service, err)
		}
		_ = ctrl.Close()
		if ctx.Err() != nil {
			return
		}
		var err error
		for delay := 200 * time.Millisecond; ; {
			ctrl, err = l.registerOnce(ctx)
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if delay < 5*time.Second {
				delay *= 2
			}
		}
		br = l.br
		logx.New("sessionnet").Infof("%s: %s tunnel re-registered", l.cfg.PIN, l.cfg.Service)
	}
}

func (l *holderListener) watchCtrl(ctx context.Context, ctrl net.Conn) chan struct{} {
	stop := make(chan struct{})
	p, ok := ctrl.(pinger)
	if !ok {
		return stop
	}
	go func() {
		t := time.NewTicker(holderPingEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pctx, cancel := context.WithTimeout(ctx, holderPingGrace)
			err := p.Ping(pctx)
			cancel()
			if err == nil {
				continue
			}
			select {
			case <-stop:
				return
			default:
			}
			logx.New("sessionnet").Warnf("%s: %s tunnel went quiet (%v) — dropping it to re-register", l.cfg.PIN, l.cfg.Service, err)
			_ = ctrl.Close()
			return
		}
	}()
	return stop
}

func (l *holderListener) pump(ctx context.Context, ctrl net.Conn, br *bufio.Reader) error {
	stop := l.watchCtrl(ctx, ctrl)
	defer close(stop)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) != 2 || f[0] != "accept" {
			continue
		}
		go l.openData(ctx, f[1])
	}
}

func (l *holderListener) openData(ctx context.Context, stream string) {
	c, err := dialRelay(ctx, l.cfg.Relay, l.cfg.PIN, l.cfg.Service, l.cfg.DialContext)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(c, "%s accept %s %s %s\n", relayProto, l.cfg.PIN, l.cfg.Service, stream); err != nil {
		_ = c.Close()
		return
	}
	sec, err := handshake(c, l.cfg.Key, false)
	if err != nil {
		logx.New("sessionnet").Warnf("%s: refused a %s connection: %v", l.cfg.PIN, l.cfg.Service, err)
		_ = c.Close()
		return
	}
	select {
	case l.conns <- sec:
	case <-ctx.Done():
		_ = sec.Close()
	case <-l.closed:
		_ = sec.Close()
	}
}

func (l *holderListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *holderListener) Close() error {
	l.once.Do(func() {
		l.cancel()
		close(l.closed)
		l.mu.Lock()
		ctrl := l.ctrl
		l.mu.Unlock()
		if ctrl != nil {
			_ = ctrl.Close()
		}
	})
	return nil
}

func (l *holderListener) Addr() net.Addr { return sessionAddr{pin: l.cfg.PIN, service: l.cfg.Service} }

func (l *holderListener) DirectAddr() string {
	if l.cfg.DirectListener == nil {
		return ""
	}
	if l.cfg.DirectAdvertise != "" {
		return l.cfg.DirectAdvertise
	}
	return l.cfg.DirectListener.Addr().String()
}

func (l *holderListener) serveDirect(ctx context.Context) {
	srv := &http.Server{Handler: relayHTTPHandler(nil, func(c net.Conn) { l.handleDirect(ctx, c) })}
	go func() { _ = srv.Serve(l.cfg.DirectListener) }()
	go func() {
		select {
		case <-ctx.Done():
		case <-l.closed:
		}
		_ = srv.Close()
	}()
	logx.New("sessionnet").Infof("%s: %s also reachable directly at %s", l.cfg.PIN, l.cfg.Service, l.DirectAddr())
}

func (l *holderListener) handleDirect(ctx context.Context, c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) != 4 || f[0] != relayProto || f[1] != "direct" {
		_ = c.Close()
		return
	}
	key, held := l.cfg.Key, f[2] == l.cfg.PIN && f[3] == l.cfg.Service
	if !held {
		key = NewKey()
	}
	if _, err := io.WriteString(c, "ok\n"); err != nil {
		_ = c.Close()
		return
	}
	sec, err := handshake(&prefixedConn{Conn: c, buf: br}, key, false)
	if err != nil || !held {
		if !held {
			logx.New("sessionnet").Warnf("%s: refused a direct connection naming %s/%s, which this holder does not serve", l.cfg.PIN, f[2], f[3])
		} else {
			logx.New("sessionnet").Warnf("%s: refused a direct %s connection: %v", l.cfg.PIN, l.cfg.Service, err)
		}
		if sec != nil {
			_ = sec.Close()
		}
		_ = c.Close()
		return
	}
	select {
	case l.conns <- sec:
	case <-ctx.Done():
		_ = sec.Close()
	case <-l.closed:
		_ = sec.Close()
	}
}

type sessionAddr struct{ pin, service string }

func (a sessionAddr) Network() string { return "club" }
func (a sessionAddr) String() string  { return a.pin + "/" + a.service }
