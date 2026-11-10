package joindaemon

import (
	"context"
	"testing"
	"time"

	"github.com/nwylynko/clubhouse-cli/conductor"
	"github.com/nwylynko/clubhouse-cli/logx"
)

func TestFleetTickIsNotBoundedByTheMirrorLoopsDeadline(t *testing.T) {
	j := &Joined{stop: make(chan struct{}), log: logx.New("join")}
	defer j.Close()

	probe := &deadlineProbe{seen: make(chan time.Duration, 1)}
	j.fleet = &conductor.Fleet{Roles: []conductor.Role{probe}}

	go j.fleetLoop()

	select {
	case budget := <-probe.seen:
		if budget < 10*time.Minute {
			t.Fatalf("the fleet ticks with %s of budget — a scaffold is killed mid-run; it must not inherit the mirror loop's deadline", budget)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the fleet never ticked")
	}
}

type deadlineProbe struct{ seen chan time.Duration }

const noDeadline = 365 * 24 * time.Hour

func (p *deadlineProbe) Name() string { return "deadline-probe" }

func (p *deadlineProbe) Tick(ctx context.Context) error {
	budget := noDeadline
	if d, ok := ctx.Deadline(); ok {
		budget = time.Until(d)
	}
	select {
	case p.seen <- budget:
	default:
	}
	return nil
}
