package conductor

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/logx"
)

type StateBoard struct {
	mu     sync.Mutex
	states map[string]controlplane.RoleAgent
}

func (b *StateBoard) Working(role, activity string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	prev := b.get(role)
	if prev.State == controlplane.RoleWorking && prev.Activity == activity {
		return
	}
	since := time.Now().UTC()
	if prev.State == controlplane.RoleWorking && !prev.Since.IsZero() {
		since = prev.Since
	}
	b.set(role, controlplane.RoleAgent{
		State: controlplane.RoleWorking, Activity: activity, Since: since,
	})
}

func (b *StateBoard) Idle(role string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.get(role).State == controlplane.RoleIdle {
		return
	}
	b.set(role, controlplane.RoleAgent{State: controlplane.RoleIdle, Since: time.Now().UTC()})
}

func (b *StateBoard) Snapshot() map[string]controlplane.RoleAgent {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now().UTC()
	out := make(map[string]controlplane.RoleAgent, len(b.states))
	for role, st := range b.states {
		st.UpdatedAt = now
		out[role] = st
	}
	return out
}

func (b *StateBoard) get(role string) controlplane.RoleAgent { return b.states[role] }

func (b *StateBoard) set(role string, st controlplane.RoleAgent) {
	if b.states == nil {
		b.states = map[string]controlplane.RoleAgent{}
	}
	b.states[role] = st
}

type StatePublisher struct {
	Control *controlplane.Client
	PIN     string
	Board   *StateBoard

	Record     controlplane.ConductorRecord
	Mechanical map[string]bool
}

func (p *StatePublisher) Publish(ctx context.Context) {
	if p == nil || p.Control == nil || p.PIN == "" {
		return
	}
	states := p.Board.Snapshot()
	if len(states) == 0 {
		return
	}
	rec := p.Record
	roles := make(map[string]controlplane.RoleAgent, len(rec.Roles)+len(states))
	for role, a := range rec.Roles {
		roles[role] = a
	}
	for role, st := range states {
		a := roles[role]
		a.State, a.Activity, a.Since, a.UpdatedAt = st.State, st.Activity, st.Since, st.UpdatedAt
		if p.Mechanical != nil {
			a.Mechanical = p.Mechanical[role]
		}
		roles[role] = a
	}
	rec.Roles = roles
	if out := p.Control.OutboxFor(p.PIN); out != nil {
		out.SetConductor(rec)
	} else if err := p.Control.PutConductor(ctx, p.PIN, rec); err != nil {
		statePubLog.Debugf("state publish failed: %v", err)
		return
	}
	statePubLog.Infof("state published — %s", describeStates(states))
}

const StatePublishInterval = 2 * time.Second

func (p *StatePublisher) Run(ctx context.Context) {
	t := time.NewTicker(StatePublishInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			for role := range p.Board.Snapshot() {
				p.Board.Idle(role)
			}
			p.Publish(context.WithoutCancel(ctx))
			return
		case <-t.C:
			p.Publish(ctx)
		}
	}
}

var statePubLog = logx.New("conductor")

func describeStates(states map[string]controlplane.RoleAgent) string {
	roles := make([]string, 0, len(states))
	for role := range states {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		line := role + " " + states[role].State
		if a := states[role].Activity; a != "" {
			line += " — " + a
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, ", ")
}
