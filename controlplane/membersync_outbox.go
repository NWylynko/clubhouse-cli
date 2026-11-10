package controlplane

import (
	"context"
	"github.com/nwylynko/clubhouse-cli/telemetry"
	"sync"
)

type Outbox struct {
	Client *Client
	PIN    string
	ID     string

	mu          sync.Mutex
	update      MemberUpdate
	generation  int
	hold        map[string]struct{}
	endpoints   map[string]EndpointPut
	convergence *Convergence
	conductor   *ConductorRecord
	cycles      int
}

func NewOutbox(c *Client, pin, id string) *Outbox {
	return &Outbox{
		Client: c, PIN: pin, ID: id,
		hold:      map[string]struct{}{},
		endpoints: map[string]EndpointPut{},
	}
}

func (c *Client) RegisterOutbox(pin string, o *Outbox) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outboxes == nil {
		c.outboxes = map[string]*Outbox{}
	}
	c.outboxes[pin] = o
}

func (c *Client) UnregisterOutbox(pin string) {
	telemetry.StopMember()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.outboxes, pin)
}

func (c *Client) OutboxFor(pin string) *Outbox {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outboxes[pin]
}

func (o *Outbox) SetMember(upd MemberUpdate) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.update = upd
}

func (o *Outbox) SetGeneration(gen int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.generation = gen
}

func (o *Outbox) SetEndpoint(kind string, put EndpointPut) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.endpoints == nil {
		o.endpoints = map[string]EndpointPut{}
	}
	o.endpoints[kind] = put
}

func (o *Outbox) SetConvergence(c Convergence) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := c
	o.convergence = &cp
}

func (o *Outbox) SetConductor(c ConductorRecord) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := c
	o.conductor = &cp
}

func (o *Outbox) Hold(services ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.hold == nil {
		o.hold = map[string]struct{}{}
	}
	for _, s := range services {
		o.hold[s] = struct{}{}
	}
}

func (o *Outbox) FlushChange(ctx context.Context) error {
	_, err := o.flush(ctx, false)
	return err
}

func (o *Outbox) Flush(ctx context.Context) (MemberSyncResult, error) {
	return o.flush(ctx, true)
}

func (o *Outbox) flush(ctx context.Context, isCycle bool) (MemberSyncResult, error) {
	o.mu.Lock()
	if isCycle {
		o.cycles++
	}
	wantSnap := isCycle && (o.cycles%SnapshotEvery == 0 || (o.Client != nil && !o.Client.StreamLive(o.PIN)))
	sync := MemberSync{
		Update:       o.update,
		Generation:   o.generation,
		WantSnapshot: wantSnap,
	}
	for s := range o.hold {
		sync.Hold = append(sync.Hold, s)
	}
	if len(o.endpoints) > 0 {
		sync.Endpoints = make(map[string]EndpointPut, len(o.endpoints))
		for k, v := range o.endpoints {
			sync.Endpoints[k] = v
		}
	}
	if o.convergence != nil {
		cp := *o.convergence
		sync.Convergence = &cp
	}
	if o.conductor != nil {
		cp := *o.conductor
		sync.Conductor = &cp
	}
	o.hold = map[string]struct{}{}
	o.endpoints = map[string]EndpointPut{}
	o.convergence = nil
	o.conductor = nil
	client := o.Client
	pin, id := o.PIN, o.ID
	o.mu.Unlock()

	if client == nil || id == "" {
		return MemberSyncResult{}, nil
	}
	return client.MemberSync(ctx, pin, id, sync)
}
