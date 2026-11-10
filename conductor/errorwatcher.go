package conductor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/logx"
)

var watcherLog = logx.New("watcher")

type Fixer func(ctx context.Context, errLog string, files map[string]string) (path string, content string, err error)

type ErrorWatcher struct {
	Host    *canonical.Host
	Logs    LogSource
	ID      sbGit.Identity
	Fix     Fixer
	Harness string
	States  *StateBoard
	Control *controlplane.Client
	PIN     string

	Settle   time.Duration
	MaxWait  time.Duration
	Cooldown time.Duration

	Health       func(ctx context.Context) error
	HealthStreak int

	Now func() time.Time

	mu           sync.Mutex
	seen         int
	pending      string
	firstPending time.Time
	lastAppend   time.Time
	lastFix      time.Time
	fixed        map[string]bool
	unhealthy    int
	healthErr    string
	healthFixed  bool
	Fixes        int
}

const (
	defaultSettle       = 5 * time.Second
	defaultMaxWait      = 30 * time.Second
	defaultCooldown     = 60 * time.Second
	defaultHealthStreak = 3
)

func (e *ErrorWatcher) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *ErrorWatcher) settle() time.Duration {
	if e.Settle > 0 {
		return e.Settle
	}
	return defaultSettle
}

func (e *ErrorWatcher) maxWait() time.Duration {
	if e.MaxWait > 0 {
		return e.MaxWait
	}
	return defaultMaxWait
}

func (e *ErrorWatcher) cooldown() time.Duration {
	if e.Cooldown > 0 {
		return e.Cooldown
	}
	return defaultCooldown
}

func (e *ErrorWatcher) healthStreak() int {
	if e.HealthStreak > 0 {
		return e.HealthStreak
	}
	return defaultHealthStreak
}

func (e *ErrorWatcher) harnessName() string {
	if e.Harness == "" {
		return "harness"
	}
	return e.Harness
}

func (e *ErrorWatcher) Name() string { return "error-watcher" }

func (e *ErrorWatcher) Tick(ctx context.Context) error {
	if e.Logs == nil || e.Host == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()

	e.pollLogs(now)
	e.pollHealth(ctx)

	chunk, fromHealth := e.due(now)
	if chunk == "" {
		return nil
	}
	if !e.lastFix.IsZero() && now.Sub(e.lastFix) < e.cooldown() {
		return nil
	}
	fp := fingerprint(chunk)
	if !fromHealth && e.fixed[fp] {
		e.pending = ""
		return nil
	}
	errLine := firstErrorLine(chunk)
	if e.Control != nil && e.PIN != "" {
		msg := errLine
		e.Control.PutConvergenceBestEffort(ctx, e.PIN, controlplane.Convergence{
			LastError: &msg, Watcher: "fixing",
		})
	}

	src := e.Host.Bare
	if e.Host.Remote != "" {
		src = e.Host.Remote
	}
	tmp, err := os.MkdirTemp("", "clubhouse-watcher-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", src, tmp); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: e.ID.EnvVars()}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}

	writePath := "clubhouse-fix.log"
	writeBody := fmt.Sprintf("auto-fix for log error:\n%s\n", firstErrorLine(chunk))
	commitMsg := "error-watcher: auto-fix"
	if e.Fix != nil {
		watcherLog.Infof("dev-server error detected — %s generating a fix: %s", e.harnessName(), firstErrorLine(chunk))
		e.States.Working(RoleErrorWatcher, e.harnessName()+" fixing: "+firstErrorLine(chunk))
		defer e.States.Idle(RoleErrorWatcher)
		p, body, ferr := e.Fix(ctx, chunk, readTree(tmp))
		if ferr != nil {
			e.bank(now, fp, fromHealth)
			watcherLog.Warnf("%s could not produce a fix: %v", e.harnessName(), ferr)
			return ferr
		}
		writePath, writeBody = p, body
		commitMsg = "error-watcher: fix " + p
		watcherLog.Infof("%s wrote a fix for %s — pushing to main", e.harnessName(), p)
	} else {
		watcherLog.Infof("dev-server error detected — no harness, writing marker file: %s", firstErrorLine(chunk))
	}

	dst := filepath.Join(tmp, writePath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, []byte(writeBody), 0o644); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", writePath); err != nil {
		return err
	}
	if err := c.Run(ctx, "diff", "--cached", "--quiet"); err == nil {
		watcherLog.Infof("%s produced no change to %s — nothing to push", e.harnessName(), writePath)
		e.bank(now, fp, fromHealth)
		return nil
	}
	if err := c.Run(ctx, "commit", "-m", commitMsg); err != nil {
		return err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return err
	}
	e.bank(now, fp, fromHealth)
	e.Fixes++
	if e.Control != nil && e.PIN != "" {
		cleared := ""
		e.Control.PutConvergenceBestEffort(ctx, e.PIN, controlplane.Convergence{
			LastError: &cleared, Watcher: "idle",
		})
	}
	return nil
}

func (e *ErrorWatcher) pollLogs(now time.Time) {
	all := e.Logs.Watchable()
	if len(all) < e.seen {
		e.seen = 0
	}
	if len(all) <= e.seen {
		return
	}
	chunk := all[e.seen:]
	e.seen = len(all)
	if strings.TrimSpace(chunk) == "" {
		return
	}
	if e.pending == "" {
		e.firstPending = now
	}
	e.pending += chunk
	e.lastAppend = now
}

func (e *ErrorWatcher) pollHealth(ctx context.Context) {
	if e.Health == nil {
		return
	}
	if err := e.Health(ctx); err != nil {
		e.unhealthy++
		e.healthErr = err.Error()
		return
	}
	e.unhealthy, e.healthErr, e.healthFixed = 0, "", false
}

func (e *ErrorWatcher) due(now time.Time) (string, bool) {
	if e.pending != "" {
		quiet := now.Sub(e.lastAppend) >= e.settle()
		waited := now.Sub(e.firstPending) >= e.maxWait()
		if quiet || waited {
			return e.pending, false
		}
	}
	if e.unhealthy >= e.healthStreak() && !e.healthFixed {
		return fmt.Sprintf("the dev server is not answering: %s\n\nlast output:\n%s",
			e.healthErr, e.pending), true
	}
	return "", false
}

func (e *ErrorWatcher) bank(now time.Time, fp string, fromHealth bool) {
	e.lastFix = now
	e.pending = ""
	if fromHealth {
		e.healthFixed = true
		e.unhealthy = 0
		return
	}
	if e.fixed == nil {
		e.fixed = map[string]bool{}
	}
	e.fixed[fp] = true
}

func fingerprint(s string) string {
	line := firstErrorLine(s)
	if line == "" || line == "ERROR: unknown" {
		for _, l := range strings.Split(s, "\n") {
			if strings.TrimSpace(l) != "" {
				line = strings.TrimSpace(l)
				break
			}
		}
	}
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(line) {
		switch {
		case r >= '0' && r <= '9':
		case r == ' ' || r == '\t':
			space = b.Len() > 0
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func readTree(root string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 64*1024 {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || !utf8Text(data) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out[rel] = string(data)
		return nil
	})
	return out
}

func utf8Text(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

var errorSignals = []string{
	"ERROR:",
	"Failed to compile",
	"Module not found",
	"SyntaxError",
	"Unexpected token",
	"Cannot find module",
	"error TS",
	"panic:",
	"Traceback (",
}

func looksLikeError(s string) bool {
	for _, sig := range errorSignals {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

func firstErrorLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		for _, sig := range errorSignals {
			if strings.Contains(line, sig) {
				return strings.TrimSpace(line)
			}
		}
	}
	return "ERROR: unknown"
}
