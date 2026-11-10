package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nwylynko/clubhouse-cli/logx"
)

const MaxBodyBytes = 64 << 10

const (
	DefaultQueueDepth = 1000
	DefaultBatchSize  = 100
	DefaultTimeout    = 2 * time.Second
)

type Event struct {
	TS time.Time

	Source     string
	PIN        string
	SessionUID string
	Member     string
	TraceID    string

	Level     string
	Component string

	Method       string
	PathTemplate string
	Status       int
	DurationMS   int64
	Bytes        int64
	Truncated    bool

	Headers string
	Body    string

	Data map[string]any
}

type Envelope struct {
	TS      time.Time `json:"ts"`
	Name    string    `json:"name"`
	Service string    `json:"service"`
	Version string    `json:"version,omitempty"`

	Source     string `json:"source,omitempty"`
	PIN        string `json:"pin,omitempty"`
	SessionUID string `json:"sessionUid,omitempty"`
	Member     string `json:"member,omitempty"`
	TraceID    string `json:"traceId,omitempty"`

	Level     string `json:"level,omitempty"`
	Component string `json:"component,omitempty"`

	Method       string `json:"method,omitempty"`
	PathTemplate string `json:"pathTemplate,omitempty"`
	Status       int    `json:"status,omitempty"`
	DurationMS   int64  `json:"durationMs,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`

	Headers string `json:"headers,omitempty"`
	Body    string `json:"body,omitempty"`

	Data map[string]any `json:"data,omitempty"`

	Dropped int64 `json:"dropped,omitempty"`
}

type Config struct {
	URL     string
	Key     string
	Bearer  string
	Service string
	Version string

	QueueDepth int
	BatchSize  int
	Timeout    time.Duration

	HTTP *http.Client
	Now  func() time.Time
}

type Emitter struct {
	cfg  Config
	log  *logx.Logger
	http *http.Client
	now  func() time.Time

	mu     sync.Mutex
	ring   []Envelope
	head   int
	count  int
	closed bool

	dropped atomic.Int64
	wake    chan struct{}
	done    chan struct{}
}

func New(cfg Config) *Emitter {
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = DefaultQueueDepth
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Service == "" {
		cfg.Service = "unknown"
	}
	e := &Emitter{
		cfg:  cfg,
		log:  logx.New("telemetry"),
		http: cfg.HTTP,
		now:  cfg.Now,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.http == nil {
		e.http = &http.Client{}
	}
	if !e.Enabled() {
		close(e.done)
		return e
	}
	e.ring = make([]Envelope, cfg.QueueDepth)
	go e.deliver()
	return e
}

func (e *Emitter) Enabled() bool { return e != nil && e.cfg.URL != "" }

func (e *Emitter) Dropped() int64 {
	if e == nil {
		return 0
	}
	return e.dropped.Load()
}

func (e *Emitter) Emit(name string, ev Event) {
	if e == nil || !e.Enabled() {
		return
	}
	if name == "" {
		e.log.Warnf("refusing an emission with no event name (source %q, pin %q)", ev.Source, ev.PIN)
		return
	}
	ts := ev.TS
	if ts.IsZero() {
		ts = e.now().UTC()
	}
	env := Envelope{
		TS: ts, Name: name, Service: e.cfg.Service, Version: e.cfg.Version,
		Source: ev.Source, PIN: ev.PIN, SessionUID: ev.SessionUID, Member: ev.Member,
		TraceID: ev.TraceID, Level: ev.Level, Component: ev.Component,
		Method: ev.Method, PathTemplate: ev.PathTemplate, Status: ev.Status,
		DurationMS: ev.DurationMS, Bytes: ev.Bytes, Truncated: ev.Truncated,
		Headers: ev.Headers, Body: ev.Body, Data: ev.Data,
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	if e.count == len(e.ring) {
		e.head = (e.head + 1) % len(e.ring)
		e.count--
		e.dropped.Add(1)
	}
	e.ring[(e.head+e.count)%len(e.ring)] = env
	e.count++
	e.mu.Unlock()

	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Emitter) Close() { e.Shutdown(context.Background()) }

func (e *Emitter) Shutdown(ctx context.Context) {
	if e == nil {
		return
	}
	e.mu.Lock()
	already := e.closed
	e.closed = true
	pending := e.count
	e.mu.Unlock()
	if !already {
		select {
		case e.wake <- struct{}{}:
		default:
		}
		if pending > 0 {
			e.log.Infof("stopping: draining %d queued telemetry envelope(s)", pending)
		}
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		e.mu.Lock()
		left := e.count
		e.count = 0
		e.mu.Unlock()
		if left > 0 {
			e.dropped.Add(int64(left))
		}
		e.log.Warnf("gave up draining telemetry after %v — %d envelope(s) undelivered; a shutdown never waits on the ingest", ctx.Err(), left)
	}
}

func (e *Emitter) deliver() {
	defer close(e.done)
	for {
		batch := e.take()
		if len(batch) > 0 {
			e.send(batch)
			continue
		}
		e.mu.Lock()
		closed := e.closed
		e.mu.Unlock()
		if closed {
			return
		}
		<-e.wake
	}
}

func (e *Emitter) take() []Envelope {
	e.mu.Lock()
	n := e.count
	if n > e.cfg.BatchSize {
		n = e.cfg.BatchSize
	}
	if n == 0 {
		e.mu.Unlock()
		return nil
	}
	batch := make([]Envelope, n)
	for i := 0; i < n; i++ {
		batch[i] = e.ring[(e.head+i)%len(e.ring)]
	}
	e.head = (e.head + n) % len(e.ring)
	e.count -= n
	e.mu.Unlock()

	if d := e.dropped.Swap(0); d > 0 {
		batch[0].Dropped = d
		e.log.Warnf("dropped %d envelope(s) — the ingest is not keeping up (queue depth %d)", d, e.cfg.QueueDepth)
	}
	return batch
}

func (e *Emitter) send(batch []Envelope) {
	body, err := json.Marshal(batch)
	if err != nil {
		e.log.Warnf("encoding a batch of %d failed, dropping it: %v", len(batch), err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.URL, bytes.NewReader(body))
	if err != nil {
		e.log.Warnf("building the ingest request failed, dropping %d: %v", len(batch), err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.Key != "" {
		req.Header.Set(KeyHeader, e.cfg.Key)
	}
	if e.cfg.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.Bearer)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		e.fail(batch, err.Error())
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		e.fail(batch, resp.Status)
		return
	}
}

func (e *Emitter) fail(batch []Envelope, why string) {
	e.dropped.Add(int64(len(batch)) + batch[0].Dropped)
	e.log.Warnf("ingest refused a batch of %d, dropping it: %s", len(batch), why)
}

const KeyHeader = "X-Club-Telemetry-Key"

const KeyEnv = "CLUB_TELEMETRY_KEY"

const URLEnv = "CLUB_TELEMETRY_URL"

func RequireURL(url string) error {
	if strings.TrimSpace(url) == "" {
		return errors.New(URLEnv + " is unset — set it to this deployment's telemetry ingest (compose uses http://clubhouse-telemetry:7779" + IngestPath + ")")
	}
	return nil
}

const IngestPath = "/v1/telemetry"

const TicketService = "telemetry"

func RequireKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New(KeyEnv + " is unset — set it in .env to the shared telemetry ingest key (any long random string; the same value on the control plane, the relay and clubhouse-telemetry)")
	}
	return nil
}

func (e *Emitter) GiveUpAfter() time.Duration {
	if e == nil {
		return 0
	}
	return e.cfg.Timeout
}
