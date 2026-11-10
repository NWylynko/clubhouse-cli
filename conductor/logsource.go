package conductor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/devserver"
)

type LogSource interface {
	Watchable() string
}

type RemoteLogSource struct {
	URL    string
	Client *http.Client

	Resolve func(context.Context) (string, error)

	Floor time.Duration

	mu       sync.Mutex
	started  bool
	lines    map[int64]devserver.Line
	order    []int64
	cursor   int64
	cancel   context.CancelFunc
	follower *devserver.Follower
}

const maxRemoteLines = 2000

func (r *RemoteLogSource) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.started = false
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *RemoteLogSource) Reconnect() {
	if r == nil {
		return
	}
	r.mu.Lock()
	f := r.follower
	r.mu.Unlock()
	f.Reconnect()
}

func (r *RemoteLogSource) floor() time.Duration {
	if r.Floor > 0 {
		return r.Floor
	}
	return controlplane.LogsFloor
}

func (r *RemoteLogSource) httpClient() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{}
}

func (r *RemoteLogSource) record(page devserver.LogPage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lines == nil {
		r.lines = map[int64]devserver.Line{}
	}
	if page.Missed > 0 {
		watcherLog.Warnf("dev-server log stream missed %d line(s) — the watcher is reading an incomplete buffer", page.Missed)
	}
	for _, l := range page.Lines {
		if _, seen := r.lines[l.Seq]; !seen {
			r.order = append(r.order, l.Seq)
		}
		r.lines[l.Seq] = l
	}
	if page.Cursor > r.cursor {
		r.cursor = page.Cursor
	}
	if len(r.order) > maxRemoteLines {
		drop := r.order[:len(r.order)-maxRemoteLines]
		r.order = r.order[len(r.order)-maxRemoteLines:]
		for _, seq := range drop {
			delete(r.lines, seq)
		}
	}
}

func (r *RemoteLogSource) Probe() error {
	if r == nil || r.URL == "" {
		return fmt.Errorf("no dev-server logs URL")
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", r.URL, resp.Status)
	}
	return nil
}

func (r *RemoteLogSource) Watchable() string {
	if r == nil || r.URL == "" {
		return ""
	}
	r.ensureFollowing()
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, seq := range r.order {
		b.WriteString(r.lines[seq].Text)
		b.WriteString("\n")
	}
	return b.String()
}

func (r *RemoteLogSource) ensureFollowing() {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()

	if page, err := r.catchUp(ctx, r.currentURL()); err == nil {
		r.record(page)
	}

	f := devserver.Follow(ctx, devserver.FollowOptions{
		URL:    r.resolveURL,
		Stream: devserver.StreamStderr,
		Phase:  devserver.PhaseDev,
		Client: r.Client,
		Floor:  r.floor(),
	}, r.reset)
	r.mu.Lock()
	r.follower = f
	r.mu.Unlock()
	go func() {
		for page := range f.C {
			r.record(page)
		}
	}()
}

func (r *RemoteLogSource) currentURL() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.URL
}

func (r *RemoteLogSource) resolveURL(ctx context.Context) (string, error) {
	if r.Resolve == nil {
		return r.currentURL(), nil
	}
	u, err := r.Resolve(ctx)
	if err != nil || u == "" {
		return r.currentURL(), err
	}
	r.mu.Lock()
	r.URL = u
	r.mu.Unlock()
	return u, nil
}

func (r *RemoteLogSource) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines, r.order, r.cursor = nil, nil, 0
}

func (r *RemoteLogSource) catchUp(ctx context.Context, base string) (devserver.LogPage, error) {
	var page devserver.LogPage
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := devserver.FollowQuery(base, 0, devserver.StreamStderr, devserver.PhaseDev)
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return page, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return page, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return page, fmt.Errorf("%s answered %s", base, resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&page)
	return page, err
}

func LogsURLFromRemote(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/logs"
}
