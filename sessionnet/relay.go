package sessionnet

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nwylynko/clubhouse-cli/clientaddr"
	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/relayticket"
	"github.com/nwylynko/clubhouse-cli/telemetry"
)

// Also implemented by cloudflare/session/src/relay-do.js in the services repo.
// Greeting verbs: register <pin> <service> | accept <pin> <service> <stream> |
// connect <pin> <service>.
const Proto = "clubhouse-relay/1"

const relayProto = Proto

const (
	defaultRegisterBurst = 30
	defaultRegisterRate  = 2 * time.Second
	knownHolderWindow    = 30 * time.Minute

	defaultConnectBurst = 60
	defaultConnectRate  = 100 * time.Millisecond

	defaultMaxSplicesPerPin     = 32
	defaultMaxLiveTunnels       = 256
	defaultMaxConcurrentSplices = 1024
	defaultSpliceBytesPerSec    = 8 << 20
	defaultHealthBurst          = 100
	defaultHealthRate           = 10 * time.Millisecond
)

const (
	holderPingEvery  = 5 * time.Second
	holderPingGrace  = 5 * time.Second
	holderAliveProbe = time.Second
)

type Relay struct {
	OnBytes func([]byte)

	Telemetry *telemetry.Emitter

	RegisterBurst int
	RegisterRate  time.Duration

	ConnectBurst int
	ConnectRate  time.Duration

	MaxSplicesPerPin int

	MaxLiveTunnels int

	MaxConcurrentSplices int

	SpliceBytesPerSec int

	ProxyHops int

	TicketPublic ed25519.PublicKey

	AllowUnauthenticated bool

	mu        sync.Mutex
	ln        net.Listener
	srv       *http.Server
	holders   map[string]*holderReg
	waiting   map[string]chan bufferedConn
	buckets   map[string]*tokenBucket
	connectB  map[string]*tokenBucket
	healthB   *tokenBucket
	known     map[string]time.Time
	streamID  uint64
	splices   int
	pinSplice map[string]int
	closed    bool
}

type holderReg struct {
	ctrl   net.Conn
	source string
	mu     sync.Mutex
}

func NewRelay() *Relay {
	return &Relay{
		holders:   map[string]*holderReg{},
		waiting:   map[string]chan bufferedConn{},
		buckets:   map[string]*tokenBucket{},
		connectB:  map[string]*tokenBucket{},
		known:     map[string]time.Time{},
		pinSplice: map[string]int{},
		ProxyHops: clientaddr.HopsFromEnv(),
	}
}

func (r *Relay) Start(addr string) error {
	if len(r.TicketPublic) == 0 && !r.AllowUnauthenticated {
		return errors.New("sessionnet: relay refuses to start with no TicketPublic — set the Ed25519 ticket public key, or AllowUnauthenticated=true for a test relay that admits anyone")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: relayHTTPHandler(r, r.handle)}
	r.mu.Lock()
	r.ln = ln
	r.srv = srv
	r.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	log := logx.New("relay")
	log.Infof("session relay listening on %s (ws %s/<pin>/<service>, health %s)", ln.Addr(), RelayPath, HealthPath)
	log.Infof("%s", clientaddr.Describe(r.ProxyHops))
	if len(r.TicketPublic) != 0 {
		log.Infof("relay tickets required (Ed25519 public key configured)")
	} else {
		log.Warnf("relay tickets NOT required (AllowUnauthenticated) — anyone may register or connect; a test relay only")
	}
	return nil
}

func (r *Relay) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return ""
	}
	return r.ln.Addr().String()
}

func (r *Relay) URL() string {
	addr := r.Addr()
	if addr == "" {
		return ""
	}
	return "ws://" + addr + RelayPath
}

func (r *Relay) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	srv := r.srv
	holders := r.holders
	r.holders = map[string]*holderReg{}
	r.mu.Unlock()
	for _, h := range holders {
		_ = h.ctrl.Close()
	}
	if srv != nil {
		return srv.Close()
	}
	return nil
}

func (r *Relay) handle(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 3 || f[0] != relayProto {
		_ = c.Close()
		return
	}
	switch f[1] {
	case "register":
		ticket, ok := takeTicketArg(f)
		if !ok {
			_ = c.Close()
			return
		}
		r.register(c, f[2], f[3], ticket)
	case "connect":
		ticket, ok := takeTicketArg(f)
		if !ok {
			_ = c.Close()
			return
		}
		r.connect(c, br, f[2], f[3], ticket)
	case "accept":
		if len(f) != 5 {
			_ = c.Close()
			return
		}
		r.acceptData(c, br, f[4])
	default:
		_ = c.Close()
	}
}

func takeTicketArg(f []string) (ticket string, ok bool) {
	switch len(f) {
	case 4:
		return "", true
	case 5:
		return f[4], true
	default:
		return "", false
	}
}

func (r *Relay) requireTicket(c net.Conn, pin, service, ticket string) (relayticket.Claims, bool) {
	if len(r.TicketPublic) == 0 {
		return relayticket.Claims{}, true
	}
	if ticket == "" {
		logx.New("relay").Warnf("%s: refused %s — ticket required", pin, service)
		r.recordDoor(eventRelayRegister, c, relayticket.Claims{PIN: pin, Service: service}, "unauthorized", time.Time{})
		_, _ = io.WriteString(c, "unauthorized\n")
		_ = c.Close()
		return relayticket.Claims{}, false
	}
	claims, err := relayticket.Verify(r.TicketPublic, ticket, time.Now())
	if err != nil || claims.PIN != pin || claims.Service != service {
		logx.New("relay").Warnf("%s: refused %s — bad ticket (%v)", pin, service, err)
		r.recordDoor(eventRelayRegister, c, relayticket.Claims{PIN: pin, Service: service}, "unauthorized", time.Time{})
		_, _ = io.WriteString(c, "unauthorized\n")
		_ = c.Close()
		return relayticket.Claims{}, false
	}
	return claims, true
}

func routeKey(pin, service string) string { return pin + "/" + service }

func sourceOf(c net.Conn) string {
	if rc, ok := c.(*relayConn); ok && rc.source != "" {
		return rc.source
	}
	a := c.RemoteAddr()
	if a == nil {
		return "unknown"
	}
	return clientaddr.PeerHost(a.String())
}

func (r *Relay) allowRegister(source, key string) bool {
	burst := r.RegisterBurst
	if burst <= 0 {
		burst = defaultRegisterBurst
	}
	rate := r.RegisterRate
	if rate <= 0 {
		rate = defaultRegisterRate
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if seen, ok := r.known[source+"|"+key]; ok && now.Sub(seen) < knownHolderWindow {
		r.known[source+"|"+key] = now
		return true
	}
	b := r.buckets[source]
	if b == nil {
		b = &tokenBucket{tokens: float64(burst), last: now}
		r.buckets[source] = b
	}
	return b.take(now, float64(burst), rate)
}

func (r *Relay) rememberKnown(source, key string) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known[source+"|"+key] = now
	r.forgetStaleKnownLocked(now)
}

func (r *Relay) allowConnect(source, key string) bool {
	burst := r.ConnectBurst
	if burst <= 0 {
		burst = defaultConnectBurst
	}
	rate := r.ConnectRate
	if rate <= 0 {
		rate = defaultConnectRate
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if seen, ok := r.known[source+"|"+key]; ok && now.Sub(seen) < knownHolderWindow {
		r.known[source+"|"+key] = now
		return true
	}
	b := r.connectB[source]
	if b == nil {
		b = &tokenBucket{tokens: float64(burst), last: now}
		r.connectB[source] = b
	}
	return b.take(now, float64(burst), rate)
}

func (r *Relay) maxSplicesPerPin() int {
	if r.MaxSplicesPerPin > 0 {
		return r.MaxSplicesPerPin
	}
	return defaultMaxSplicesPerPin
}

func (r *Relay) maxLiveTunnels() int {
	if r.MaxLiveTunnels > 0 {
		return r.MaxLiveTunnels
	}
	return defaultMaxLiveTunnels
}

func (r *Relay) maxConcurrentSplices() int {
	if r.MaxConcurrentSplices > 0 {
		return r.MaxConcurrentSplices
	}
	return defaultMaxConcurrentSplices
}

func (r *Relay) spliceBytesPerSec() int {
	if r.SpliceBytesPerSec > 0 {
		return r.SpliceBytesPerSec
	}
	return defaultSpliceBytesPerSec
}

func (r *Relay) takeSpliceSlot(pin string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.splices >= r.maxConcurrentSplices() {
		return false
	}
	if r.pinSplice[pin] >= r.maxSplicesPerPin() {
		return false
	}
	r.splices++
	r.pinSplice[pin]++
	return true
}

func (r *Relay) releaseSpliceSlot(pin string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.splices > 0 {
		r.splices--
	}
	if n := r.pinSplice[pin]; n <= 1 {
		delete(r.pinSplice, pin)
	} else {
		r.pinSplice[pin] = n - 1
	}
}

func (r *Relay) allowHealth() bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.healthB == nil {
		r.healthB = &tokenBucket{tokens: float64(defaultHealthBurst), last: now}
	}
	return r.healthB.take(now, float64(defaultHealthBurst), defaultHealthRate)
}

func holderAlive(reg *holderReg) bool {
	if reg == nil || reg.ctrl == nil {
		return false
	}
	p, ok := reg.ctrl.(pinger)
	if !ok {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), holderAliveProbe)
	defer cancel()
	return p.Ping(ctx) == nil
}

func (r *Relay) forgetStaleKnownLocked(now time.Time) {
	if len(r.known) < 4096 {
		return
	}
	for k, seen := range r.known {
		if now.Sub(seen) >= knownHolderWindow {
			delete(r.known, k)
		}
	}
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func (b *tokenBucket) take(now time.Time, burst float64, rate time.Duration) bool {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() / rate.Seconds()
		if b.tokens > burst {
			b.tokens = burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *Relay) register(c net.Conn, pin, service, ticket string) {
	started := time.Now()
	claims, ok := r.requireTicket(c, pin, service, ticket)
	if !ok {
		return
	}
	claims.PIN, claims.Service = pin, service
	key := routeKey(pin, service)
	source := sourceOf(c)

	r.mu.Lock()
	old := r.holders[key]
	closed := r.closed
	r.mu.Unlock()
	if closed {
		_ = c.Close()
		return
	}
	if old != nil && holderAlive(old) {
		logx.New("relay").Warnf("%s: refused %s registration from %s — holder still alive", pin, service, source)
		r.recordDoor(eventRelayRegister, c, claims, "busy", started)
		_, _ = io.WriteString(c, "busy\n")
		_ = c.Close()
		return
	}

	if !r.allowRegister(source, key) {
		logx.New("relay").Warnf("%s: refused %s registration from %s — rate limited", pin, service, source)
		r.recordDoor(eventRelayRegister, c, claims, "rate-limited", started)
		_, _ = io.WriteString(c, "rate-limited\n")
		_ = c.Close()
		return
	}
	reg := &holderReg{ctrl: c, source: source}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = c.Close()
		return
	}
	if cur := r.holders[key]; cur != nil && cur != old {
		r.mu.Unlock()
		if holderAlive(cur) {
			logx.New("relay").Warnf("%s: refused %s registration from %s — holder still alive", pin, service, source)
			r.recordDoor(eventRelayRegister, c, claims, "busy", started)
			_, _ = io.WriteString(c, "busy\n")
			_ = c.Close()
			return
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			_ = c.Close()
			return
		}
		if r.holders[key] != cur && r.holders[key] != nil {
			r.mu.Unlock()
			logx.New("relay").Warnf("%s: refused %s registration from %s — another holder claimed the slot", pin, service, source)
			r.recordDoor(eventRelayRegister, c, claims, "busy", started)
			_, _ = io.WriteString(c, "busy\n")
			_ = c.Close()
			return
		}
		old = r.holders[key]
	}
	if old == nil && len(r.holders) >= r.maxLiveTunnels() {
		r.mu.Unlock()
		logx.New("relay").Warnf("%s: refused %s registration from %s — live tunnel ceiling", pin, service, source)
		r.recordDoor(eventRelayRegister, c, claims, "full", started)
		_, _ = io.WriteString(c, "full\n")
		_ = c.Close()
		return
	}
	r.holders[key] = reg
	r.mu.Unlock()
	if old != nil {
		_ = old.ctrl.Close()
	}
	r.rememberKnown(source, key)
	if _, err := io.WriteString(c, "ok\n"); err != nil {
		_ = c.Close()
		return
	}
	r.recordDoor(eventRelayRegister, c, claims, "ok", started)
	logx.New("relay").Infof("%s: %s tunnel registered from %s", pin, service, source)

	stop := r.watchHolder(key, reg)

	buf := make([]byte, 1)
	for {
		if _, err := c.Read(buf); err != nil {
			break
		}
	}
	close(stop)
	r.mu.Lock()
	if r.holders[key] == reg {
		delete(r.holders, key)
	}
	r.mu.Unlock()
	_ = c.Close()
	logx.New("relay").Infof("%s: %s tunnel closed", pin, service)
}

func (r *Relay) watchHolder(key string, reg *holderReg) chan struct{} {
	stop := make(chan struct{})
	p, ok := reg.ctrl.(pinger)
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
			case <-t.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), holderPingGrace)
			err := p.Ping(ctx)
			cancel()
			if err == nil {
				continue
			}
			select {
			case <-stop:
				return
			default:
			}
			logx.New("relay").Warnf("%s: holder stopped answering (%v) — evicting so a reconnect can take over", key, err)
			_ = reg.ctrl.Close()
			return
		}
	}()
	return stop
}

func (r *Relay) connect(client net.Conn, br *bufio.Reader, pin, service, ticket string) {
	started := time.Now()
	claims, ok := r.requireTicket(client, pin, service, ticket)
	if !ok {
		return
	}
	claims.PIN, claims.Service = pin, service
	key := routeKey(pin, service)
	source := sourceOf(client)
	if !r.allowConnect(source, key) {
		logx.New("relay").Warnf("%s: refused %s connect from %s — rate limited", pin, service, source)
		r.recordDoor(eventRelayConnect, client, claims, "rate-limited", started)
		_, _ = io.WriteString(client, "rate-limited\n")
		_ = client.Close()
		return
	}
	if !r.takeSpliceSlot(pin) {
		logx.New("relay").Warnf("%s: refused %s connect from %s — splice ceiling", pin, service, source)
		r.recordDoor(eventRelayConnect, client, claims, "full", started)
		_, _ = io.WriteString(client, "full\n")
		_ = client.Close()
		return
	}
	defer r.releaseSpliceSlot(pin)

	r.mu.Lock()
	reg := r.holders[key]
	if reg == nil {
		r.mu.Unlock()
		r.recordDoor(eventRelayConnect, client, claims, "no-holder", started)
		_, _ = io.WriteString(client, "no-holder\n")
		_ = client.Close()
		return
	}
	r.streamID++
	stream := fmt.Sprintf("%s-%d", pin, r.streamID)
	ch := make(chan bufferedConn, 1)
	r.waiting[stream] = ch
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.waiting, stream)
		r.mu.Unlock()
	}()

	reg.mu.Lock()
	_, err := io.WriteString(reg.ctrl, fmt.Sprintf("accept %s\n", stream))
	reg.mu.Unlock()
	if err != nil {
		r.recordDoor(eventRelayConnect, client, claims, "no-holder", started)
		_, _ = io.WriteString(client, "no-holder\n")
		_ = client.Close()
		return
	}

	select {
	case up := <-ch:
		if _, err := io.WriteString(client, "ok\n"); err != nil {
			_ = up.conn.Close()
			_ = client.Close()
			return
		}
		r.recordDoor(eventRelayConnect, client, claims, "ok", started)
		spliceStart := time.Now()
		moved := r.splice(client, br, up.conn, up.buf)
		r.recordSplice(client, claims, moved, spliceStart)
	case <-time.After(15 * time.Second):
		r.recordDoor(eventRelayConnect, client, claims, "no-holder", started)
		_, _ = io.WriteString(client, "no-holder\n")
		_ = client.Close()
	}
}

type bufferedConn struct {
	conn net.Conn
	buf  *bufio.Reader
}

func (r *Relay) acceptData(c net.Conn, br *bufio.Reader, stream string) {
	r.mu.Lock()
	ch := r.waiting[stream]
	r.mu.Unlock()
	if ch == nil {
		_ = c.Close()
		return
	}
	select {
	case ch <- bufferedConn{conn: c, buf: br}:
	default:
		_ = c.Close()
	}
}

func (r *Relay) splice(a net.Conn, aBuf *bufio.Reader, b net.Conn, bBuf *bufio.Reader) int64 {
	defer a.Close()
	defer b.Close()
	bps := r.spliceBytesPerSec()
	var moved atomic.Int64
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(b, r.pace(r.observe(aBuf), bps))
		moved.Add(n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(a, r.pace(r.observe(bBuf), bps))
		moved.Add(n)
		done <- struct{}{}
	}()
	<-done
	return moved.Load()
}

func (r *Relay) observe(src io.Reader) io.Reader {
	if r.OnBytes == nil {
		return src
	}
	return &observedReader{src: src, fn: r.OnBytes}
}

func (r *Relay) pace(src io.Reader, bytesPerSec int) io.Reader {
	if bytesPerSec <= 0 {
		return src
	}
	return &pacedReader{src: src, bps: bytesPerSec, last: time.Now(), tokens: float64(bytesPerSec)}
}

type pacedReader struct {
	src    io.Reader
	bps    int
	tokens float64
	last   time.Time
}

func (p *pacedReader) Read(b []byte) (int, error) {
	now := time.Now()
	if elapsed := now.Sub(p.last); elapsed > 0 {
		p.tokens += elapsed.Seconds() * float64(p.bps)
		if p.tokens > float64(p.bps) {
			p.tokens = float64(p.bps)
		}
		p.last = now
	}
	if p.tokens < 1 {
		wait := time.Duration((1 - p.tokens) / float64(p.bps) * float64(time.Second))
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		time.Sleep(wait)
		return p.Read(b)
	}
	max := int(p.tokens)
	if max > len(b) {
		max = len(b)
	}
	if max > p.bps/10 && p.bps/10 > 0 {
		max = p.bps / 10
	}
	n, err := p.src.Read(b[:max])
	if n > 0 {
		p.tokens -= float64(n)
	}
	return n, err
}

type observedReader struct {
	src io.Reader
	fn  func([]byte)
}

func (o *observedReader) Read(p []byte) (int, error) {
	n, err := o.src.Read(p)
	if n > 0 {
		b := make([]byte, n)
		copy(b, p[:n])
		o.fn(b)
	}
	return n, err
}

const (
	eventRelayRegister    = "relay.register"
	eventRelayConnect     = "relay.connect"
	eventRelaySpliceClose = "relay.splice.close"
)

func (r *Relay) recordDoor(name string, c net.Conn, claims relayticket.Claims, outcome string, started time.Time) {
	if r.Telemetry == nil {
		return
	}
	var ms int64
	if !started.IsZero() {
		ms = time.Since(started).Milliseconds()
	}
	r.Telemetry.Emit(name, telemetry.Event{
		Source: sourceOf(c), PIN: claims.PIN, SessionUID: claims.SessionUID,
		Member: claims.MemberID, DurationMS: ms,
		Data: map[string]any{"service": claims.Service, "outcome": outcome},
	})
}

func (r *Relay) recordSplice(c net.Conn, claims relayticket.Claims, moved int64, started time.Time) {
	if r.Telemetry == nil {
		return
	}
	r.Telemetry.Emit(eventRelaySpliceClose, telemetry.Event{
		Source: sourceOf(c), PIN: claims.PIN, SessionUID: claims.SessionUID,
		Member: claims.MemberID,
		Bytes:  moved, DurationMS: time.Since(started).Milliseconds(),
		Data: map[string]any{"service": claims.Service},
	})
}
