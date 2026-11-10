package conductor

import (
	"context"
	"fmt"
	"strings"

	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/harness"
	"github.com/nwylynko/clubhouse-cli/logx"
)

const (
	RoleMerger       = "merger"
	RoleSetup        = "setup"
	RoleErrorWatcher = "error-watcher"
)

type SessionFleetSpec struct {
	Host    *canonical.Host
	ID      sbGit.Identity
	Control *controlplane.Client
	PIN     string

	Fallback *harness.Client
}

type SessionFleet struct {
	Fleet *Fleet

	Harnesses map[string]string

	LogsURL string

	Logs *RemoteLogSource

	DevURL string

	States     *StateBoard
	Publisher  *StatePublisher
	Mechanical map[string]bool
}

func BuildSessionFleet(ctx context.Context, spec SessionFleetSpec) SessionFleet {
	log := logx.New("conductor")
	agents := SessionRoleAgents(ctx, spec.Control, spec.PIN)
	brainFor := func(role string) *harness.Client {
		a, ok := agents[role]
		if !ok || a.Harness == "" {
			return withDir(spec.Fallback, spec.Host.Work)
		}
		if !harness.IsAvailable(a.Harness) {
			log.Warnf("the session runs %s with %s, which is not installed here — that role runs mechanically", role, a.Harness)
			return nil
		}
		return withDir(harness.Lookup(a.Harness, a.Model), spec.Host.Work)
	}

	built := SessionFleet{Harnesses: map[string]string{}, States: &StateBoard{}, Mechanical: map[string]bool{}}
	mechanical := func(role string, brain *harness.Client) {
		a, ok := agents[role]
		built.Mechanical[role] = ok && a.Harness != "" && brain == nil
	}
	mergerBrain := brainFor(RoleMerger)
	mechanical(RoleMerger, mergerBrain)
	merger := &Merger{
		Host: spec.Host, ID: spec.ID, Resolve: HarnessResolver(mergerBrain),
		Harness: HarnessNameOf(mergerBrain), Control: spec.Control, PIN: spec.PIN,
		States: built.States,
	}
	built.Harnesses[RoleMerger] = merger.Harness

	setupBrain := brainFor(RoleSetup)
	mechanical(RoleSetup, setupBrain)
	setup := &Setup{
		Host: spec.Host, ID: spec.ID, Agent: HarnessScaffolder(setupBrain),
		Harness: HarnessNameOf(setupBrain), Control: spec.Control, PIN: spec.PIN,
		States: built.States,
	}
	built.Harnesses[RoleSetup] = setup.Harness

	roles := []Role{merger}
	if built.LogsURL = SessionLogsURL(ctx, spec.Control, spec.PIN, spec.Host); built.LogsURL != "" {
		watchBrain := brainFor(RoleErrorWatcher)
		mechanical(RoleErrorWatcher, watchBrain)
		built.DevURL = SessionDevURL(ctx, spec.Control, spec.PIN, spec.Host)
		logs := &RemoteLogSource{
			URL: built.LogsURL,
			Resolve: func(ctx context.Context) (string, error) {
				if u := SessionLogsURL(ctx, spec.Control, spec.PIN, spec.Host); u != "" {
					return u, nil
				}
				return "", fmt.Errorf("session %s has no logs endpoint", spec.PIN)
			},
		}
		built.Logs = logs
		watcher := &ErrorWatcher{
			Host: spec.Host, Logs: logs,
			ID: spec.ID, Fix: HarnessFixer(watchBrain), Harness: HarnessNameOf(watchBrain),
			Control: spec.Control, PIN: spec.PIN,
			Health: HTTPHealth(built.DevURL),
			States: built.States,
		}
		roles = append(roles, watcher)
		built.Harnesses[RoleErrorWatcher] = watcher.Harness
	}
	built.Fleet = &Fleet{Roles: roles, Detached: []Role{setup}}
	for role := range built.Harnesses {
		built.States.Idle(role)
	}
	built.Publisher = &StatePublisher{
		Control: spec.Control, PIN: spec.PIN, Board: built.States,
		Record:     SessionRecord(ctx, spec.Control, spec.PIN, agents),
		Mechanical: built.Mechanical,
	}
	return built
}

func SessionRecord(ctx context.Context, client *controlplane.Client, pin string, agents map[string]controlplane.RoleAgent) controlplane.ConductorRecord {
	rec := controlplane.ConductorRecord{Active: true, Roles: agents}
	if client == nil || pin == "" {
		return rec
	}
	if sess, err := client.Session(ctx, pin); err == nil && sess.Conductor != nil {
		rec = *sess.Conductor
		if len(rec.Roles) == 0 {
			rec.Roles = agents
		}
	}
	return rec
}

func (s SessionFleet) HasIntelligence() bool {
	for _, h := range s.Harnesses {
		if h != "" {
			return true
		}
	}
	return false
}

func (s SessionFleet) Summary() string {
	var b strings.Builder
	for _, role := range []string{RoleMerger, RoleSetup, RoleErrorWatcher} {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(role + "=")
		switch h, built := s.Harnesses[role]; {
		case !built:
			b.WriteString("off")
		case h == "":
			b.WriteString("mechanical")
		default:
			b.WriteString(h)
		}
	}
	return b.String()
}

func SessionRoleAgents(ctx context.Context, client *controlplane.Client, pin string) map[string]controlplane.RoleAgent {
	if client == nil {
		return nil
	}
	sess, err := client.Session(ctx, pin)
	if err != nil || sess.Conductor == nil || len(sess.Conductor.Roles) == 0 {
		return nil
	}
	return sess.Conductor.Roles
}

func SessionLogsURL(ctx context.Context, client *controlplane.Client, pin string, host *canonical.Host) string {
	if client != nil {
		if url, err := client.EndpointURL(ctx, pin, controlplane.EndpointLogs); err == nil && url != "" {
			return url
		}
	}
	if host == nil {
		return ""
	}
	return LogsURLFromRemote(host.RemoteURL())
}

func HarnessNameOf(c *harness.Client) string {
	if c == nil {
		return ""
	}
	return string(c.Name)
}
