package conductor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/durability"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/syncengine"
)

var mergerLog = logx.New("merger")

type Role interface {
	Name() string
	Tick(ctx context.Context) error
}

type Fleet struct {
	Roles    []Role
	Detached []Role
	After    []Role

	mu       sync.Mutex
	inFlight map[string]bool
}

var fleetLog = logx.New("fleet")

func (f *Fleet) TickRoles(ctx context.Context) error {
	f.startDetached(ctx)
	return f.runRoles(ctx, f.Roles)
}

func (f *Fleet) TickRolesToCompletion(ctx context.Context) error {
	all := make([]Role, 0, len(f.Roles)+len(f.Detached))
	all = append(all, f.Roles...)
	for _, r := range f.Detached {
		if f.claim(r.Name()) {
			defer f.release(r.Name())
			all = append(all, r)
		}
	}
	return f.runRoles(ctx, all)
}

func (f *Fleet) startDetached(ctx context.Context) {
	for _, r := range f.Detached {
		if !f.claim(r.Name()) {
			fleetLog.Debugf("%s is still running from an earlier tick — not starting it again", r.Name())
			continue
		}
		go func(r Role) {
			defer f.release(r.Name())
			if err := r.Tick(ctx); err != nil {
				fleetLog.Warnf("%s: %v", r.Name(), err)
			}
		}(r)
	}
}

func (f *Fleet) runRoles(ctx context.Context, roles []Role) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(roles))
	for _, r := range roles {
		wg.Add(1)
		go func(r Role) {
			defer wg.Done()
			if err := r.Tick(ctx); err != nil {
				errCh <- fmt.Errorf("%s: %w", r.Name(), err)
			}
		}(r)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	return nil
}

func (f *Fleet) claim(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inFlight[name] {
		return false
	}
	if f.inFlight == nil {
		f.inFlight = map[string]bool{}
	}
	f.inFlight[name] = true
	return true
}

func (f *Fleet) release(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.inFlight, name)
}

func (f *Fleet) TickAfter(ctx context.Context) error {
	for _, r := range f.After {
		if err := r.Tick(ctx); err != nil {
			return fmt.Errorf("%s: %w", r.Name(), err)
		}
	}
	return nil
}

func (f *Fleet) TickAll(ctx context.Context) error {
	if err := f.TickRolesToCompletion(ctx); err != nil {
		return err
	}
	return f.TickAfter(ctx)
}

type ConflictResolver func(ctx context.Context, workDir, branch, intent string, conflicted []string) (map[string]string, error)

type Merger struct {
	Host         *canonical.Host
	ID           sbGit.Identity
	Resolve      ConflictResolver
	Harness      string
	Mirror       *durability.Mirror
	HarnessCalls atomic.Int64
	Control      *controlplane.Client
	PIN          string
	States       *StateBoard
}

func (m *Merger) harnessName() string {
	if m.Harness == "" {
		return "harness"
	}
	return m.Harness
}

func (m *Merger) Name() string { return "merger" }

func (m *Merger) Tick(ctx context.Context) error {
	ahead, err := m.Host.BranchesAheadOfMain(ctx)
	if err != nil {
		return err
	}
	if len(ahead) > 0 {
		m.States.Working(RoleMerger, "merging "+ahead[0])
		err := m.mergeBranch(ctx, ahead[0])
		m.States.Idle(RoleMerger)
		if err != nil {
			return err
		}
	}
	if err := m.sweepMainMarkers(ctx); err != nil {
		return err
	}
	m.publishConvergence(ctx)
	return nil
}

func (m *Merger) mergeBranch(ctx context.Context, branch string) error {
	c := &sbGit.Cmd{Dir: m.Host.Work, Env: m.ID.EnvVars()}
	if err := c.Run(ctx, "fetch", "origin"); err != nil {
		return err
	}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}
	if err := c.Run(ctx, "reset", "--hard", "origin/main"); err != nil {
		return err
	}

	intent := m.readIntent(ctx, branch)
	err := c.Run(ctx, "merge", "--no-edit", "origin/"+branch)
	if err == nil {
		if err := c.Run(ctx, "push", "origin", "main"); err != nil {
			return err
		}
		mergerLog.Infof("merged %s into main (clean, no AI needed)", branch)
		m.publishApplied(ctx, branch, intent, 0)
		m.afterMainAdvanced()
		return nil
	}

	conflicted := listConflicted(ctx, m.Host.Work)
	if m.Resolve != nil {
		mergerLog.Infof("conflict merging %s — %s resolving %d file(s): %s",
			branch, m.harnessName(), len(conflicted), strings.Join(conflicted, ", "))
		m.States.Working(RoleMerger, fmt.Sprintf("%s resolving %d conflict(s) on %s",
			m.harnessName(), len(conflicted), branch))
		m.HarnessCalls.Add(1)
		files, rerr := m.Resolve(ctx, m.Host.Work, branch, intent, conflicted)
		if rerr != nil {
			mergerLog.Warnf("%s could not resolve %s: %v — aborting this merge, will retry next tick",
				m.harnessName(), branch, rerr)
			_ = c.Run(ctx, "merge", "--abort")
			return rerr
		}
		for path, body := range files {
			if werr := writeFile(m.Host.Work, path, body); werr != nil {
				_ = c.Run(ctx, "merge", "--abort")
				return werr
			}
			_ = c.Run(ctx, "add", path)
		}
		mergerLog.Infof("%s resolved %d file(s) on %s — committing to main", m.harnessName(), len(files), branch)
	} else {
		mergerLog.Infof("conflict merging %s — no harness available, taking the incoming side for %d file(s): %s",
			branch, len(conflicted), strings.Join(conflicted, ", "))
		for _, path := range conflicted {
			_ = c.Run(ctx, "checkout", "--theirs", path)
			_ = c.Run(ctx, "add", path)
		}
	}
	msg := fmt.Sprintf("merge %s into main", branch)
	if intent != "" {
		msg += "\n\nClub-Intent: " + intent
	}
	if err := c.Run(ctx, "commit", "-m", msg); err != nil {
		_ = c.Run(ctx, "merge", "--abort")
		return err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return err
	}
	m.publishApplied(ctx, branch, intent, len(conflicted))
	m.afterMainAdvanced()
	return nil
}

func (m *Merger) sweepMainMarkers(ctx context.Context) error {
	c := &sbGit.Cmd{Dir: m.Host.Work, Env: m.ID.EnvVars()}
	if err := c.Run(ctx, "fetch", "origin"); err != nil {
		return err
	}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}
	if err := c.Run(ctx, "reset", "--hard", "origin/main"); err != nil {
		return err
	}
	marked := conflictMarkerFiles(ctx, m.Host.Work)
	if len(marked) == 0 {
		return nil
	}

	if m.Resolve != nil {
		mergerLog.Infof("main carries leftover conflict markers in %d file(s): %s — %s healing",
			len(marked), strings.Join(marked, ", "), m.harnessName())
		m.States.Working(RoleMerger, fmt.Sprintf("%s healing conflict markers on main (%d file(s))",
			m.harnessName(), len(marked)))
		defer m.States.Idle(RoleMerger)
		m.HarnessCalls.Add(1)
		files, rerr := m.Resolve(ctx, m.Host.Work, "main (committed conflict markers)", "", marked)
		if rerr != nil {
			mergerLog.Warnf("%s could not heal markers: %v — will retry next tick", m.harnessName(), rerr)
			return rerr
		}
		for path, body := range files {
			if werr := writeFile(m.Host.Work, path, body); werr != nil {
				return werr
			}
			_ = c.Run(ctx, "add", path)
		}
	} else {
		mergerLog.Infof("main carries leftover conflict markers in %d file(s): %s — no harness, taking the incoming side",
			len(marked), strings.Join(marked, ", "))
		for _, path := range marked {
			body, err := readWorkFile(m.Host.Work, path)
			if err != nil {
				return err
			}
			if werr := writeFile(m.Host.Work, path, stripConflictMarkers(body)); werr != nil {
				return werr
			}
			_ = c.Run(ctx, "add", path)
		}
	}

	if still := conflictMarkerFiles(ctx, m.Host.Work); len(still) > 0 {
		_ = c.Run(ctx, "reset", "--hard", "origin/main")
		return fmt.Errorf("markers remain after heal in %s; not pushing", strings.Join(still, ", "))
	}
	if err := c.Run(ctx, "commit", "-am", "conductor: heal leftover conflict markers on main"); err != nil {
		return err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return err
	}
	mergerLog.Infof("healed conflict markers on main (%d file(s))", len(marked))
	m.afterMainAdvanced()
	return nil
}

func (m *Merger) publishApplied(ctx context.Context, branch, intent string, conflicts int) {
	if m.Control == nil || m.PIN == "" {
		return
	}
	sha, _ := sbGit.Output(ctx, m.Host.Bare, "rev-parse", "refs/heads/main")
	m.Control.PublishEventBestEffort(ctx, m.PIN, controlplane.EventMergeApplied, map[string]any{
		"branch": branch, "intent": intent, "sha": strings.TrimSpace(sha), "conflicts": conflicts,
	})
}

func (m *Merger) afterMainAdvanced() {
	ctx := context.Background()
	if m.Mirror != nil {
		m.Mirror.Trigger(ctx)
	}
	m.publishConvergence(ctx)
	if out := m.Control.OutboxFor(m.PIN); out != nil {
		if err := out.FlushChange(ctx); err != nil {
			mergerLog.Debugf("could not publish main.advanced immediately: %v", err)
		}
	}
}

func (m *Merger) publishConvergence(ctx context.Context) {
	if m.Control == nil || m.PIN == "" || m.Host == nil {
		return
	}
	mainSHA, _ := sbGit.Output(ctx, m.Host.Bare, "rev-parse", "refs/heads/main")
	ahead, _ := m.Host.BranchesAheadOfMain(ctx)
	ahead, omitted := controlplane.TruncateBranchesAhead(ahead)
	sha := strings.TrimSpace(mainSHA)
	m.publishConvergenceRecord(ctx, controlplane.Convergence{
		MainSHA: sha, BranchesAhead: ahead, BranchesAheadOmitted: omitted,
	})
	mergerLog.Infof("tick — %d branch(es) ahead, main %s", len(ahead), shortSHA(sha))
}

func (m *Merger) publishConvergenceRecord(ctx context.Context, c controlplane.Convergence) {
	if out := m.Control.OutboxFor(m.PIN); out != nil {
		out.SetConvergence(c)
		return
	}
	m.Control.PutConvergenceBestEffort(ctx, m.PIN, c)
}

func (m *Merger) readIntent(ctx context.Context, branch string) string {
	out, err := sbGit.Output(ctx, m.Host.Bare, "log", "-1", "--format=%B", branch)
	if err != nil {
		return ""
	}
	return syncengine.ReadIntent(out)
}

func listConflicted(ctx context.Context, work string) []string {
	out, err := sbGit.Output(ctx, work, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

func writeFile(root, rel, body string) error {
	return writeFileOS(root, rel, body)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
