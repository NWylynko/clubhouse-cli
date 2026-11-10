package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

type streamState struct {
	cancel   context.CancelFunc
	started  bool
	lastByte time.Time
	mu       sync.Mutex
	subs     []chan SessionUpdate
}

func (st *streamState) sawBytes() {
	st.mu.Lock()
	st.lastByte = time.Now()
	st.mu.Unlock()
}

func (st *streamState) silent() {
	st.mu.Lock()
	st.lastByte = time.Time{}
	st.mu.Unlock()
}

type SessionUpdate struct {
	Session Session
	Events  []Event
}

func (c *Client) Watch(ctx context.Context, pin string) {
	c.mu.Lock()
	if c.watching == nil {
		c.watching = map[string]*streamState{}
	}
	st := c.watching[pin]
	if st == nil {
		st = &streamState{}
		c.watching[pin] = st
	}
	if st.started {
		c.mu.Unlock()
		return
	}
	wctx, cancel := context.WithCancel(ctx)
	st.started, st.cancel = true, cancel
	c.mu.Unlock()

	go c.watchLoop(wctx, pin, st)
}

func (c *Client) CloseWatch(pin string) {
	c.mu.Lock()
	st := c.watching[pin]
	if st != nil {
		delete(c.watching, pin)
	}
	c.mu.Unlock()
	if st != nil {
		if st.cancel != nil {
			st.cancel()
		}
		st.mu.Lock()
		for _, ch := range st.subs {
			close(ch)
		}
		st.subs = nil
		st.mu.Unlock()
	}
}

func (c *Client) Updates(pin string) <-chan SessionUpdate {
	ch := make(chan SessionUpdate, 8)
	c.mu.Lock()
	if c.watching == nil {
		c.watching = map[string]*streamState{}
	}
	st := c.watching[pin]
	if st == nil {
		st = &streamState{}
		c.watching[pin] = st
	}
	st.mu.Lock()
	st.subs = append(st.subs, ch)
	st.mu.Unlock()
	c.mu.Unlock()
	return ch
}

const maxFoldedEvents = 1000

func (st *streamState) publish(u SessionUpdate) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, ch := range st.subs {
		next := u
		for {
			select {
			case ch <- next:
			default:
				select {
				case old := <-ch:
					merged := make([]Event, 0, len(old.Events)+len(next.Events))
					merged = append(merged, old.Events...)
					merged = append(merged, next.Events...)
					if len(merged) > maxFoldedEvents {
						merged = merged[len(merged)-maxFoldedEvents:]
					}
					next = SessionUpdate{Session: next.Session, Events: merged}
				default:
				}
				continue
			}
			break
		}
	}
}

func (c *Client) watchingPin(pin string) bool {
	c.mu.Lock()
	st := c.watching[pin]
	c.mu.Unlock()
	return st != nil && st.started
}

func (c *Client) silentAfter() time.Duration {
	if c.StreamSilentAfter > 0 {
		return c.StreamSilentAfter
	}
	return StreamSilentAfter
}

func (c *Client) StreamLive(pin string) bool {
	c.mu.Lock()
	st := c.watching[pin]
	c.mu.Unlock()
	if st == nil {
		return false
	}
	st.mu.Lock()
	last := st.lastByte
	st.mu.Unlock()
	return !last.IsZero() && time.Since(last) < c.silentAfter()
}

func (c *Client) watchLoop(ctx context.Context, pin string, st *streamState) {
	var since int64
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			st.silent()
			return
		}
		next, err := c.watchOnce(ctx, pin, since, st)
		if ctx.Err() != nil {
			st.silent()
			return
		}
		st.silent()
		if err == nil {
			since = next
			backoff = time.Second
			continue
		}
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrNoSession) {
			return
		}
		jitter := time.Duration(rand.Int63n(int64(backoff/2) + 1))
		sleep := backoff/2 + jitter
		if sleep > 30*time.Second {
			sleep = 30 * time.Second
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) WatchOnce(ctx context.Context, pin string, since int64) (int64, error) {
	return c.watchOnce(ctx, pin, since, &streamState{})
}

func (c *Client) watchOnce(ctx context.Context, pin string, since int64, st *streamState) (int64, error) {
	httpClient := &http.Client{Timeout: 0}
	if c.HTTP != nil && c.HTTP.Transport != nil {
		httpClient.Transport = c.HTTP.Transport
	}
	reqCtx, dropSilent := context.WithCancel(ctx)
	defer dropSilent()
	req, err := newVersionedRequest(reqCtx, http.MethodGet,
		fmt.Sprintf("%s/v1/sessions/%s/events?since=%d", c.Base, pin, since), nil)
	if err != nil {
		return since, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.attachBearer(req, "/v1/sessions/"+pin+"/events")
	res, err := httpClient.Do(req)
	if err != nil {
		return since, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUpgradeRequired {
		b, _ := io.ReadAll(res.Body)
		return since, upgradeRequired("the session stream", b)
	}
	if res.StatusCode == 404 {
		return since, fmt.Errorf("%w", ErrNoSession)
	}
	if res.StatusCode == 401 {
		return since, fmt.Errorf("%w", ErrUnauthorized)
	}
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(res.Body)
		return since, fmt.Errorf("watch %s: %s", pin, stringsTrim(string(b)))
	}
	st.sawBytes()

	silent := c.silentAfter()
	watchdog := time.NewTicker(silent / 3)
	defer watchdog.Stop()
	go func() {
		for {
			select {
			case <-reqCtx.Done():
				return
			case <-watchdog.C:
				st.mu.Lock()
				last := st.lastByte
				st.mu.Unlock()
				if !last.IsZero() && time.Since(last) >= silent {
					dropSilent()
					return
				}
			}
		}
	}()

	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event, data string
	last := since
	for sc.Scan() {
		line := sc.Text()
		st.sawBytes()
		switch {
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if event == "session" && data != "" {
				var frame struct {
					Session Session `json:"session"`
					Events  []Event `json:"events"`
				}
				if err := json.Unmarshal([]byte(data), &frame); err == nil && frame.Session.PIN != "" {
					c.mu.Lock()
					c.cache[pin] = frame.Session
					c.mu.Unlock()
					for _, e := range frame.Events {
						if e.Seq > last {
							last = e.Seq
						}
					}
					st.publish(SessionUpdate{Session: frame.Session, Events: frame.Events})
				}
			} else if data != "" {
				var e Event
				if err := json.Unmarshal([]byte(data), &e); err == nil && e.Seq > last {
					last = e.Seq
				}
			}
			event, data = "", ""
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return last, err
	}
	return last, io.EOF
}
