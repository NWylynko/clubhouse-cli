package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/nwylynko/clubhouse-cli/sessionnet"
	"github.com/nwylynko/clubhouse-cli/telemetry"
)

var controlClientLog = logx.New("control-client")

var ErrDemoted = errors.New("demoted to client: stale generation")

var ErrUnavailable = errors.New("control plane unreachable")

var ErrNoSession = errors.New("session does not exist (unknown or expired pin)")

var ErrUnauthorized = errors.New("unauthorized")

var ErrBadRequest = errors.New("bad request")

var ErrUpgradeRequired = errors.New("club is too old for this control plane — re-run install.sh, then rejoin")

var ErrRateLimited = errors.New("rate limited")

var (
	ErrDoorClosed = errors.New("session is closed")
	ErrDoorFull   = errors.New("session is full")
)

type Client struct {
	Base              string
	HTTP              *http.Client
	mu                sync.Mutex
	cache             map[string]Session
	buf               []bufferedWrite
	maxBuf            int
	watching          map[string]*streamState
	outboxes          map[string]*Outbox
	secrets           map[string]membership
	tickets           map[string]map[string]string
	StreamSilentAfter time.Duration
}

type bufferedWrite struct {
	method, path string
	body         []byte
}

func NewClient(base string) *Client {
	return &Client{
		Base:    trimSlash(base),
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		cache:   map[string]Session{},
		maxBuf:  64,
		secrets: map[string]membership{},
	}
}

func (c *Client) RememberMembership(pin, memberID, secret string) {
	pin = strings.TrimSpace(pin)
	memberID, secret = strings.TrimSpace(memberID), strings.TrimSpace(secret)
	if pin == "" || secret == "" || memberID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets == nil {
		c.secrets = map[string]membership{}
	}
	c.secrets[pin] = membership{id: memberID, secret: secret}
}

func (c *Client) RememberRelayTickets(pin string, tickets map[string]string) {
	pin = strings.TrimSpace(pin)
	if pin == "" || len(tickets) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tickets == nil {
		c.tickets = map[string]map[string]string{}
	}
	cp := make(map[string]string, len(tickets))
	for k, v := range tickets {
		if k != "" && v != "" {
			cp[k] = v
		}
	}
	if len(cp) == 0 {
		return
	}
	c.tickets[pin] = cp
	_ = session.WriteRelayTickets(pin, cp)
}

func (c *Client) RelayTicket(pin, service string) string {
	c.mu.Lock()
	if c.tickets != nil {
		if tok := c.tickets[pin][service]; tok != "" {
			c.mu.Unlock()
			return tok
		}
	}
	c.mu.Unlock()
	disk := session.ReadRelayTickets(pin)
	if len(disk) == 0 {
		return ""
	}
	c.mu.Lock()
	if c.tickets == nil {
		c.tickets = map[string]map[string]string{}
	}
	c.tickets[pin] = disk
	tok := disk[service]
	c.mu.Unlock()
	return tok
}

func (c *Client) DialTicket(pin string) func(service string) (string, error) {
	return func(service string) (string, error) {
		return c.RelayTicket(pin, service), nil
	}
}

func (c *Client) HolderTicket(pin, service string) func() (string, error) {
	return func() (string, error) {
		return c.RelayTicket(pin, service), nil
	}
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) Session(ctx context.Context, pin string) (Session, error) {
	if c.watchingPin(pin) {
		c.mu.Lock()
		cached, ok := c.cache[pin]
		c.mu.Unlock()
		if ok {
			return cached, nil
		}
	}
	var s Session
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+pin, nil, &s)
	if err != nil {
		c.mu.Lock()
		cached, ok := c.cache[pin]
		c.mu.Unlock()
		if ok {
			return cached, fmt.Errorf("%w (using cache): %v", ErrUnavailable, err)
		}
		return s, err
	}
	c.mu.Lock()
	c.cache[pin] = s
	c.mu.Unlock()
	return s, nil
}

func (c *Client) Cached(pin string) (Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.cache[pin]
	return s, ok
}

func (c *Client) Claim(ctx context.Context, req ClaimRequest) (ClaimResponse, error) {
	var res ClaimResponse
	err := c.doWithPin(ctx, http.MethodPost, "/v1/sessions", req.PIN, req, &res)
	if err == nil {
		c.mu.Lock()
		c.cache[res.Session.PIN] = res.Session
		c.mu.Unlock()
		if res.Secret != "" {
			if err := session.WriteMembership(res.Session.PIN, res.MemberID, res.Secret); err != nil {
				return res, fmt.Errorf("persist membership: %w", err)
			}
			c.RememberMembership(res.Session.PIN, res.MemberID, res.Secret)
		}
		c.RememberRelayTickets(res.Session.PIN, res.RelayTickets)
		mid := res.MemberID
		if mid == "" {
			mid = c.memberIDFor(res.Session.PIN)
		}
		c.useTelemetry(res.Session.PIN, mid, &res.Session)
	}
	return res, err
}

func (c *Client) PutEndpoint(ctx context.Context, pin, kind string, put EndpointPut) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/endpoints/"+kind, put, nil)
	if err == nil {
		c.cacheEndpoint(pin, kind, put)
	}
	return err
}

func (c *Client) invalidate(pin string) {
	c.mu.Lock()
	delete(c.cache, pin)
	c.mu.Unlock()
}

func (c *Client) cacheEndpoint(pin, kind string, put EndpointPut) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sess, ok := c.cache[pin]
	if !ok {
		return
	}
	if put.URL == "" {
		if sess.Endpoints != nil {
			delete(sess.Endpoints, kind)
			c.cache[pin] = sess
		}
		return
	}
	eps := make(map[string]Endpoint, len(sess.Endpoints)+1)
	for k, v := range sess.Endpoints {
		eps[k] = v
	}
	now := time.Now().UTC()
	eps[kind] = Endpoint{
		URL: put.URL, Host: put.Host, Port: put.Port,
		Source: put.Source, Direct: put.Direct, UpdatedAt: &now,
	}
	sess.Endpoints = eps
	c.cache[pin] = sess
}

func (c *Client) ClearEndpoint(ctx context.Context, pin, kind string, generation int) error {
	return c.PutEndpoint(ctx, pin, kind, EndpointPut{URL: "", Generation: generation, Source: "clear"})
}

func (c *Client) PutEndpointBestEffort(ctx context.Context, pin, kind string, put EndpointPut) {
	if err := c.PutEndpoint(ctx, pin, kind, put); err != nil && !errors.Is(err, ErrDemoted) {
		c.buffer(http.MethodPut, "/v1/sessions/"+pin+"/endpoints/"+kind, put)
	}
}

func (c *Client) Cutover(ctx context.Context, pin string, req CutoverRequest) (Session, error) {
	var s Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/cutover", req, &s)
	if err == nil {
		c.mu.Lock()
		c.cache[pin] = s
		c.mu.Unlock()
	}
	return s, err
}

type InviteResult struct {
	MemberID     string
	Secret       string
	RelayTickets map[string]string
}

func (c *Client) Invite(ctx context.Context, pin string, req MemberJoinRequest) (InviteResult, error) {
	var out struct {
		MemberID     string            `json:"memberId"`
		Secret       string            `json:"secret"`
		RelayTickets map[string]string `json:"relayTickets"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/members", req, &out)
	if err == nil {
		c.invalidate(pin)
	}
	return InviteResult{MemberID: out.MemberID, Secret: out.Secret, RelayTickets: out.RelayTickets}, err
}

func (c *Client) SetAccess(ctx context.Context, pin, access string) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/access", AccessPut{Access: access}, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

func (c *Client) PendingMembers(ctx context.Context, pin string) ([]Member, error) {
	var out []Member
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+pin+"/pending", nil, &out)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Member{}
	}
	return out, nil
}

func (c *Client) DecideMember(ctx context.Context, pin, memberID, decision string) (Member, error) {
	var m Member
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/members/"+memberID+"/decision",
		MemberDecision{Decision: decision}, &m)
	if err == nil {
		c.invalidate(pin)
	}
	return m, err
}

var ErrDeclined = errors.New("join request declined")

var ErrKnockSpent = errors.New("this join request was already decided and its secret is gone")

func (c *Client) Redeem(ctx context.Context, pin string, req RedeemRequest, onPending func(RedeemPending)) (RedeemResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return RedeemResult{}, err
	}
	httpReq, err := newVersionedRequest(ctx, http.MethodPost, c.Base+"/v1/sessions/"+pin+"/redeem", bytes.NewReader(body))
	if err != nil {
		return RedeemResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	hc := c.http()
	stream := *hc
	stream.Timeout = 0
	res, err := stream.Do(httpReq)
	if err != nil {
		return RedeemResult{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUpgradeRequired {
		b, _ := io.ReadAll(res.Body)
		return RedeemResult{}, upgradeRequired("redeem", b)
	}
	if res.StatusCode == 404 {
		b, _ := io.ReadAll(res.Body)
		msg := stringsTrim(string(b))
		low := strings.ToLower(msg)
		switch {
		case strings.Contains(low, "closed"):
			return RedeemResult{}, fmt.Errorf("%w: %s", ErrDoorClosed, msg)
		case strings.Contains(low, "full"):
			return RedeemResult{}, fmt.Errorf("%w: %s", ErrDoorFull, msg)
		default:
			return RedeemResult{}, fmt.Errorf("%w: %s", ErrNoSession, msg)
		}
	}
	if res.StatusCode == http.StatusConflict {
		b, _ := io.ReadAll(res.Body)
		return RedeemResult{}, fmt.Errorf("%w: %s", ErrKnockSpent, stringsTrim(string(b)))
	}
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(res.Body)
		msg := stringsTrim(string(b))
		if msg == "" {
			msg = res.Status
		}
		return RedeemResult{}, fmt.Errorf("redeem: %s", msg)
	}
	result, err := readRedeemSSE(res.Body, onPending)
	if err != nil {
		return RedeemResult{}, err
	}
	switch result.Decision {
	case "accepted":
		if result.Secret != "" {
			if err := session.WriteMembership(pin, result.MemberID, result.Secret); err != nil {
				return result, fmt.Errorf("persist membership: %w", err)
			}
			c.RememberMembership(pin, result.MemberID, result.Secret)
		}
		c.RememberRelayTickets(pin, result.RelayTickets)
		c.mu.Lock()
		c.cache[pin] = result.Session
		c.mu.Unlock()
		return result, nil
	case "declined":
		msg := result.Message
		if msg == "" {
			msg = "join request declined"
		}
		return result, fmt.Errorf("%w: %s", ErrDeclined, msg)
	default:
		return result, fmt.Errorf("redeem: unexpected decision %q", result.Decision)
	}
}

func readRedeemSSE(r io.Reader, onPending func(RedeemPending)) (RedeemResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event string
	var data strings.Builder
	flush := func() (RedeemResult, bool, error) {
		payload := data.String()
		ev := event
		event, data = "", strings.Builder{}
		switch ev {
		case "pending":
			if onPending != nil {
				var p RedeemPending
				if err := json.Unmarshal([]byte(payload), &p); err == nil {
					onPending(p)
				}
			}
			return RedeemResult{}, false, nil
		case "accepted", "declined":
			var out RedeemResult
			if err := json.Unmarshal([]byte(payload), &out); err != nil {
				return RedeemResult{}, false, err
			}
			if out.Decision == "" {
				out.Decision = ev
			}
			return out, true, nil
		default:
			return RedeemResult{}, false, nil
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "data: "):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(line[6:])
		case line == "":
			if out, ok, err := flush(); ok || err != nil {
				return out, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return RedeemResult{}, err
	}
	if out, ok, err := flush(); ok || err != nil {
		return out, err
	}
	return RedeemResult{}, fmt.Errorf("redeem: stream ended without a decision")
}

func (c *Client) JoinMember(ctx context.Context, pin string, req MemberJoinRequest) (string, error) {
	var out struct {
		MemberID     string            `json:"memberId"`
		Secret       string            `json:"secret"`
		RelayTickets map[string]string `json:"relayTickets"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/members", req, &out)
	if err == nil {
		c.invalidate(pin)
		if out.Secret != "" {
			if err := session.WriteMembership(pin, out.MemberID, out.Secret); err != nil {
				return out.MemberID, fmt.Errorf("persist membership: %w", err)
			}
			c.RememberMembership(pin, out.MemberID, out.Secret)
		}
		c.RememberRelayTickets(pin, out.RelayTickets)
	}
	return out.MemberID, err
}

func (c *Client) Heartbeat(ctx context.Context, pin, id string, upd MemberUpdate) error {
	return c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/members/"+id, upd, nil)
}

func (c *Client) MemberSync(ctx context.Context, pin, id string, sync MemberSync) (MemberSyncResult, error) {
	var res MemberSyncResult
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/members/"+id+"/sync", sync, &res)
	if err != nil {
		return res, err
	}
	c.RememberRelayTickets(pin, res.RelayTickets)
	c.useTelemetry(pin, id, res.Session)
	if res.Session != nil {
		c.mu.Lock()
		c.cache[pin] = *res.Session
		c.mu.Unlock()
		return res, nil
	}
	if !res.Conflict {
		for kind, put := range sync.Endpoints {
			c.cacheEndpoint(pin, kind, put)
		}
	}
	return res, nil
}

func (c *Client) LeaveMember(ctx context.Context, pin, id string) error {
	err := c.do(ctx, http.MethodDelete, "/v1/sessions/"+pin+"/members/"+id, nil, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

func (c *Client) EndSession(ctx context.Context, pin string) error {
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/end", nil, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

func (c *Client) PutConvergence(ctx context.Context, pin string, conv Convergence) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/convergence", conv, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

func (c *Client) PutConvergenceBestEffort(ctx context.Context, pin string, conv Convergence) {
	if err := c.PutConvergence(ctx, pin, conv); err != nil {
		c.buffer(http.MethodPut, "/v1/sessions/"+pin+"/convergence", conv)
	}
}

func (c *Client) PutConductor(ctx context.Context, pin string, rec ConductorRecord) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/conductor", rec, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

func (c *Client) PutBox(ctx context.Context, pin string, b BoxFacts) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/box", b, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

func (c *Client) GitURL(ctx context.Context, pin string) (string, int, error) {
	s, err := c.Session(ctx, pin)
	if err != nil && !errors.Is(err, ErrUnavailable) {
		return "", 0, err
	}
	if errors.Is(err, ErrUnavailable) {
	}
	ep, ok := s.Endpoints[EndpointGit]
	if !ok || ep.URL == "" {
		return "", s.Generation, fmt.Errorf("no git endpoint for %s", pin)
	}
	url, err := c.Dialable(ctx, s, ep.URL)
	if err != nil {
		return "", s.Generation, err
	}
	return url, s.Generation, nil
}

func (c *Client) Dialable(ctx context.Context, s Session, raw string) (string, error) {
	if !sessionnet.IsSessionURL(raw) {
		return raw, nil
	}
	key, err := sessionnet.ParseKey(s.SessionKey)
	if err != nil {
		return "", err
	}
	if key.Zero() {
		return "", fmt.Errorf("session %s published %s but handed out no session key — nothing can authenticate to it", s.PIN, raw)
	}
	if s.RelayAddr == "" {
		return "", fmt.Errorf("session %s is on the session network but its control plane names no relay (start one with `club relay`)", s.PIN)
	}
	return sessionnet.LocalURL(ctx, raw, s.RelayAddr, key, directFor(s), c.DialTicket(s.PIN))
}

type NoEndpointError struct{ PIN, Kind string }

func (e *NoEndpointError) Error() string {
	return "publishes no " + e.Kind + " endpoint"
}

func (c *Client) EndpointURL(ctx context.Context, pin, kind string) (string, error) {
	sess, err := c.Session(ctx, pin)
	if err != nil {
		return "", err
	}
	ep, ok := sess.Endpoints[kind]
	if !ok || ep.URL == "" {
		return "", fmt.Errorf("session %s: %w", pin, &NoEndpointError{PIN: pin, Kind: kind})
	}
	return c.Dialable(ctx, sess, ep.URL)
}

func (c *Client) SessionDialer(ctx context.Context, pin string) (*sessionnet.Dialer, error) {
	sess, err := c.Session(ctx, pin)
	if err != nil {
		return nil, err
	}
	key, err := sessionnet.ParseKey(sess.SessionKey)
	if err != nil {
		return nil, err
	}
	if key.Zero() {
		return nil, fmt.Errorf("session %s has no session key — cannot reach services over the session network", pin)
	}
	if sess.RelayAddr == "" {
		return nil, fmt.Errorf("session %s has no relay — cannot reach services over the session network", pin)
	}
	return &sessionnet.Dialer{
		Relay:  sess.RelayAddr,
		PIN:    pin,
		Key:    key,
		Direct: directFor(sess),
		Ticket: c.DialTicket(pin),
	}, nil
}

func directFor(s Session) func(service string) string {
	return func(service string) string {
		ep, ok := s.Endpoints[service]
		if !ok {
			return ""
		}
		return ep.Direct
	}
}

func (c *Client) RequestBox(ctx context.Context, pin string, req BoxRequest) (BoxFacts, error) {
	var b BoxFacts
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/box", req, &b)
	return b, err
}

func (c *Client) GetBox(ctx context.Context, pin string) (BoxFacts, error) {
	var b BoxFacts
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+pin+"/box", nil, &b)
	return b, err
}

func (c *Client) DestroyBox(ctx context.Context, pin string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sessions/"+pin+"/box", nil, nil)
}

func (c *Client) WaitForBox(ctx context.Context, pin string, timeout time.Duration) (BoxFacts, error) {
	deadline := time.Now().Add(timeout)
	var last BoxFacts
	for {
		b, err := c.GetBox(ctx, pin)
		if err == nil {
			last = b
			switch b.State {
			case BoxReady:
				return b, nil
			case BoxFailed:
				return b, fmt.Errorf("provisioning the box for %s failed: %s", pin, b.Error)
			}
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("the box for %s was still %q after %s", pin, orState(last.State), timeout)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func orState(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (c *Client) PublishEvent(ctx context.Context, pin, kind string, payload map[string]any) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/events",
		EventPost{Kind: kind, Payload: payload}, nil)
}

func (c *Client) PublishEventBestEffort(ctx context.Context, pin, kind string, payload map[string]any) {
	path := "/v1/sessions/" + pin + "/events"
	e := EventPost{Kind: kind, Payload: payload}
	if err := c.do(ctx, http.MethodPost, path, e, nil); err != nil {
		if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrBadRequest) {
			return
		}
		c.buffer(http.MethodPost, path, e)
	}
}

func (c *Client) EventsSince(ctx context.Context, pin string, since int64) ([]Event, error) {
	var events []Event
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/sessions/%s/events?since=%d", pin, since), nil, &events)
	return events, err
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/healthz", nil, &h)
	return h, err
}

func (c *Client) WaitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		reqCtx, cancel := context.WithTimeout(ctx, time.Second)
		h, err := c.Health(reqCtx)
		cancel()
		if err == nil && h.OK {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("server reported not-ok")
			}
			return fmt.Errorf("control plane not healthy after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (c *Client) FlushBuffer(ctx context.Context) {
	c.mu.Lock()
	buf := c.buf
	c.buf = nil
	c.mu.Unlock()
	for _, w := range buf {
		req, err := newVersionedRequest(ctx, w.method, c.Base+w.path, bytes.NewReader(w.body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		c.attachBearer(req, w.path)
		res, err := c.http().Do(req)
		if err != nil {
			c.bufferRaw(w.method, w.path, w.body)
			continue
		}
		res.Body.Close()
	}
}

func (c *Client) buffer(method, path string, body any) {
	b, _ := json.Marshal(body)
	c.bufferRaw(method, path, b)
}

func (c *Client) bufferRaw(method, path string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) >= c.maxBuf {
		c.buf = c.buf[1:]
	}
	c.buf = append(c.buf, bufferedWrite{method: method, path: path, body: body})
}

func upgradeRequired(path string, body []byte) error {
	if detail := stringsTrim(string(body)); detail != "" {
		controlClientLog.Debugf("%s refused this build: %s", path, strings.ReplaceAll(detail, "\n", " "))
	}
	return ErrUpgradeRequired
}

func newVersionedRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(VersionHeader, ClientVersion)
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithPin(ctx, method, path, "", body, out)
}

func (c *Client) doWithPin(ctx context.Context, method, path, pinHint string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := newVersionedRequest(ctx, method, c.Base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	pin := pinHint
	if pin == "" {
		pin = pinFromAPIPath(path)
	}
	c.attachBearerForPin(req, pin)
	res, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusUpgradeRequired {
		return upgradeRequired(path, b)
	}
	if res.StatusCode == 409 {
		return ErrDemoted
	}
	if res.StatusCode == 404 {
		return fmt.Errorf("%w: %s", ErrNoSession, stringsTrim(string(b)))
	}
	if res.StatusCode == 401 {
		return fmt.Errorf("%w: %s", ErrUnauthorized, stringsTrim(string(b)))
	}
	if res.StatusCode == 400 {
		msg := stringsTrim(string(b))
		if msg == "" {
			msg = res.Status
		}
		return fmt.Errorf("%w: %s", ErrBadRequest, msg)
	}
	if res.StatusCode == 429 {
		msg := stringsTrim(string(b))
		if msg == "" {
			msg = res.Status
		}
		return fmt.Errorf("%w: %s", ErrRateLimited, msg)
	}
	if res.StatusCode >= 300 {
		msg := stringsTrim(string(b))
		if msg == "" {
			msg = res.Status
		}
		return fmt.Errorf("%s: %s", path, msg)
	}
	if out == nil || res.StatusCode == 204 || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func (c *Client) attachBearer(req *http.Request, path string) {
	c.attachBearerForPin(req, pinFromAPIPath(path))
}

func (c *Client) attachBearerForPin(req *http.Request, pin string) {
	if pin == "" {
		return
	}
	if c != nil {
		c.mu.Lock()
		m := c.secrets[pin]
		c.mu.Unlock()
		if m.secret != "" && m.id != "" {
			setMemberAuth(req, m.id, m.secret)
			return
		}
	}
	if id, secret := session.ReadMembership(pin); secret != "" && id != "" {
		setMemberAuth(req, id, secret)
	}
}

func (c *Client) memberIDFor(pin string) string {
	if c != nil {
		c.mu.Lock()
		m := c.secrets[pin]
		c.mu.Unlock()
		if m.id != "" {
			return m.id
		}
	}
	id, _ := session.ReadMembership(pin)
	return id
}

func setMemberAuth(req *http.Request, memberID, secret string) {
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set(MemberHeader, memberID)
}

type membership struct{ id, secret string }

func pinFromAPIPath(path string) string {
	const prefix = "/v1/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func stringsTrim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

func (c *Client) useTelemetry(pin, id string, sess *Session) {
	advertised := ""
	uid := ""
	if sess != nil {
		advertised, uid = sess.TelemetryURL, sess.UID
	} else if cached, ok := c.Cached(pin); ok {
		advertised, uid = cached.TelemetryURL, cached.UID
	}
	telemetry.UseMember(telemetry.MemberConfig{
		PIN: pin, SessionUID: uid, MemberID: id,
		Advertised: advertised,
		Ticket:     c.RelayTicket(pin, telemetry.TicketService),
		Version:    ClientVersion,
	})
}
