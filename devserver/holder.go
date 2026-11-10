package devserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/sessionnet"
)

type HolderOptions struct {
	Relay           string
	PIN             string
	Key             sessionnet.Key
	LocalPort       int
	DirectListener  net.Listener
	DirectAdvertise string
	Ticket          func() (string, error)
}

type Holder struct {
	ln     net.Listener
	url    string
	direct string
	cancel context.CancelFunc
	once   sync.Once

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func StartHolder(ctx context.Context, opt HolderOptions) (*Holder, error) {
	if opt.Relay == "" {
		return nil, errors.New("devserver: no relay address")
	}
	if opt.LocalPort <= 0 {
		return nil, errors.New("devserver: no local dev port to publish")
	}
	ctx, cancel := context.WithCancel(ctx)
	ln, err := sessionnet.Serve(ctx, sessionnet.HolderConfig{
		Relay: opt.Relay, PIN: opt.PIN, Service: "dev", Key: opt.Key,
		DirectListener: opt.DirectListener, DirectAdvertise: opt.DirectAdvertise,
		Ticket: opt.Ticket,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("publish the dev server onto the session network: %w", err)
	}
	h := &Holder{
		ln: ln, cancel: cancel, conns: map[net.Conn]struct{}{},
		url:    sessionnet.FormatURL(opt.PIN, "dev", ""),
		direct: sessionnet.DirectAddr(ln),
	}
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(opt.LocalPort))
	go h.accept(ctx, target)
	logx.New("dev").Infof("dev server published on the session network: %s → %s", h.url, target)
	return h, nil
}

func (h *Holder) URL() string { return h.url }

func (h *Holder) Direct() string { return h.direct }

func (h *Holder) Close() error {
	if h == nil {
		return nil
	}
	h.once.Do(func() {
		h.cancel()
		_ = h.ln.Close()
		h.mu.Lock()
		for c := range h.conns {
			_ = c.Close()
		}
		h.conns = map[net.Conn]struct{}{}
		h.mu.Unlock()
	})
	return nil
}

func (h *Holder) accept(ctx context.Context, target string) {
	log := logx.New("dev")
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		h.conns[c] = struct{}{}
		h.mu.Unlock()
		go func() {
			defer func() {
				h.mu.Lock()
				delete(h.conns, c)
				h.mu.Unlock()
				c.Close()
			}()
			var d net.Dialer
			up, err := d.DialContext(ctx, "tcp", target)
			if err != nil {
				log.Warnf("dev server at %s did not answer a session-network request: %v", target, err)
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
			<-done
		}()
	}
}
