package devserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type FollowOptions struct {
	URL func(context.Context) (string, error)

	Stream Stream
	Phase  Phase

	Client *http.Client

	Floor time.Duration

	OnStatus func(error)
}

type Follower struct {
	C <-chan LogPage

	cancel context.CancelFunc
	kick   chan struct{}
}

func (f *Follower) Close() {
	if f == nil || f.cancel == nil {
		return
	}
	f.cancel()
}

func (f *Follower) Reconnect() {
	if f == nil || f.kick == nil {
		return
	}
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

func Follow(ctx context.Context, opt FollowOptions, onReset func()) *Follower {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan LogPage, 32)
	f := &Follower{C: ch, cancel: cancel, kick: make(chan struct{}, 1)}
	go func() {
		defer close(ch)
		var lastURL string
		var cursor int64
		floor := opt.Floor
		if floor <= 0 {
			floor = 5 * time.Second
		}
		for {
			if ctx.Err() != nil {
				return
			}
			base, err := opt.resolve(ctx)
			if err == nil && base != "" {
				if base != lastURL {
					if lastURL != "" && onReset != nil {
						onReset()
					}
					lastURL, cursor = base, 0
				}
				cursor, err = followOnce(ctx, f.kick, opt, base, cursor, ch)
			}
			if opt.OnStatus != nil && ctx.Err() == nil {
				opt.OnStatus(err)
			}
			select {
			case <-ctx.Done():
				return
			case <-f.kick:
			case <-time.After(floor):
			}
		}
	}()
	return f
}

func (o FollowOptions) resolve(ctx context.Context) (string, error) {
	if o.URL == nil {
		return "", fmt.Errorf("no logs URL")
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return o.URL(rctx)
}

func (o FollowOptions) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{}
}

func FollowQuery(base string, cursor int64, stream Stream, phase Phase) string {
	q := fmt.Sprintf("%s?since=%d", strings.TrimSuffix(base, "/"), cursor)
	if stream != "" {
		q += "&stream=" + string(stream)
	}
	if phase != "" {
		q += "&phase=" + string(phase)
	}
	return q
}

func followOnce(ctx context.Context, kick <-chan struct{}, opt FollowOptions, base string, cursor int64, out chan<- LogPage) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-kick:
			cancel()
		case <-ctx.Done():
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, FollowQuery(base, cursor, opt.Stream, opt.Phase), nil)
	if err != nil {
		return cursor, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := opt.client().Do(req)
	if err != nil {
		return cursor, fmt.Errorf("dev-server logs at %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cursor, fmt.Errorf("dev-server logs at %s: %s", base, resp.Status)
	}

	deliver := func(page LogPage) {
		if page.Cursor > cursor {
			cursor = page.Cursor
		}
		if len(page.Lines) == 0 && page.Missed == 0 {
			return
		}
		select {
		case out <- page:
		case <-ctx.Done():
		}
	}

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		var page LogPage
		if derr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&page); derr != nil {
			return cursor, fmt.Errorf("dev-server logs at %s: %w", base, derr)
		}
		deliver(page)
		return cursor, nil
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if data != "" {
				var page LogPage
				if err := json.Unmarshal([]byte(data), &page); err == nil {
					deliver(page)
				}
			}
			data = ""
		}
		if ctx.Err() != nil {
			return cursor, nil
		}
	}
	return cursor, nil
}
