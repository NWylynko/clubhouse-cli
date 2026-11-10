package cutover

import (
	"context"
	"fmt"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/syncengine"
)

var log = logx.New("cutover")

type Options struct {
	PIN         string
	NewDialAddr string
	LogsURL     string
	OverlayAddr string
	Control     *controlplane.Client
	Generation  int
	HostMachine string
	FromURL     string
	Drain       bool
}

type Result struct {
	DialAddr   string
	Generation int
	CaughtUp   bool
	Drained    bool
}

func Flip(ctx context.Context, opt Options) (*Result, error) {
	if opt.PIN == "" || opt.NewDialAddr == "" {
		return nil, fmt.Errorf("cutover: PIN and NewDialAddr required")
	}
	if opt.Control == nil {
		return nil, fmt.Errorf("cutover: need Control client")
	}
	if opt.Generation <= 0 {
		sess, err := opt.Control.Session(ctx, opt.PIN)
		if err != nil {
			return nil, fmt.Errorf("cutover: resolve generation: %w", err)
		}
		opt.Generation = sess.Generation
	}
	res := &Result{DialAddr: opt.NewDialAddr, Drained: opt.Drain}
	if opt.FromURL != "" && !sameURL(opt.FromURL, opt.NewDialAddr) {
		if opt.Drain {
			log.Infof("drain: refreshing %s from %s before flip", opt.NewDialAddr, opt.FromURL)
		} else {
			log.Infof("catch-up: pushing main %s → %s before flip", opt.FromURL, opt.NewDialAddr)
		}
		if err := syncengine.CatchUp(ctx, opt.FromURL, opt.NewDialAddr); err != nil {
			return nil, fmt.Errorf("cutover catch-up: %w", err)
		}
		res.CaughtUp = true
	}
	sess, err := opt.Control.Cutover(ctx, opt.PIN, controlplane.CutoverRequest{
		NewGitURL:   opt.NewDialAddr,
		From:        opt.FromURL,
		Drain:       opt.Drain,
		Generation:  opt.Generation,
		LogsURL:     opt.LogsURL,
		HostMachine: opt.HostMachine,
	})
	if err != nil {
		return nil, fmt.Errorf("cutover: %w", err)
	}
	res.Generation = sess.Generation
	log.Infof("control plane flipped: %s → %s (gen %d; clients follow on next sync)",
		opt.PIN, opt.NewDialAddr, sess.Generation)
	return res, nil
}

func sameURL(a, b string) bool {
	trim := func(s string) string {
		for len(s) > 0 && s[len(s)-1] == '/' {
			s = s[:len(s)-1]
		}
		return s
	}
	return trim(a) == trim(b)
}
