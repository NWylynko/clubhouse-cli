package conductor

import (
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/controlplane"
)

type RefreshGate struct {
	Remote bool
	Floor  time.Duration
	Frames <-chan controlplane.SessionUpdate

	StreamLive func() bool

	mu   sync.Mutex
	last time.Time
}

func NewRefreshGate(client *controlplane.Client, pin string, remote bool) *RefreshGate {
	g := &RefreshGate{Remote: remote, Floor: controlplane.RemoteConductorFloor}
	if remote && client != nil && pin != "" {
		g.Frames = client.Updates(pin)
		g.StreamLive = func() bool { return client.StreamLive(pin) }
	}
	return g
}

func (g *RefreshGate) Due(now time.Time) bool {
	if g == nil || !g.Remote {
		return true
	}
	pushed := g.drain()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last.IsZero() || pushed || now.Sub(g.last) >= g.floor() {
		g.last = now
		return true
	}
	return false
}

func (g *RefreshGate) floor() time.Duration {
	f := g.Floor
	if f <= 0 {
		f = controlplane.RemoteConductorFloor
	}
	live := g.StreamLive != nil && g.StreamLive()
	return controlplane.Floor(f, live)
}

func (g *RefreshGate) drain() bool {
	pushed := false
	for {
		select {
		case u, ok := <-g.Frames:
			if !ok {
				g.Frames = nil
				return pushed
			}
			for _, e := range u.Events {
				switch e.Kind {
				case controlplane.EventSyncPushed, controlplane.EventMergeApplied:
					pushed = true
				}
			}
		default:
			return pushed
		}
	}
}
