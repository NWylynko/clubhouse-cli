package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/console"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/reach"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/nwylynko/clubhouse-cli/syncengine"
	"github.com/nwylynko/clubhouse-cli/update"
	"github.com/spf13/cobra"
)

type consoleSession struct {
	PIN      string
	Me       string
	MemberID string
	Branch   string
	WorkPath string
	Elector  bool
	Leave    func(context.Context) error
	Work     func(ctx context.Context, out io.Writer) error
	Announce *announcer
}

type leaver struct {
	mu     sync.Mutex
	close  func()
	closed bool
}

func (l *leaver) holds(close func()) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		close()
		return
	}
	l.close = close
	l.mu.Unlock()
}

func (l *leaver) Leave(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.close != nil {
		l.close()
	}
	return nil
}

type announcer struct {
	ch   chan console.MemberMsg
	path atomic.Pointer[string]
}

func newAnnouncer() *announcer { return &announcer{ch: make(chan console.MemberMsg, 8)} }

func (a *announcer) Announce(msg console.MemberMsg) {
	if a == nil {
		return
	}
	if msg.WorkPath != "" {
		p := msg.WorkPath
		a.path.Store(&p)
	}
	select {
	case a.ch <- msg:
	default:
	}
}

func (a *announcer) WorkPath() string {
	if a == nil {
		return ""
	}
	if p := a.path.Load(); p != nil {
		return *p
	}
	return ""
}

func runConsole(ctx context.Context, cmd *cobra.Command, cs consoleSession) error {
	updateNudge := update.StartCheck(ctx, Version)
	defer func() {
		if line := updateNudge(); line != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), line)
		}
	}()

	if entered := testHooks().ConsoleEntered; entered != nil {
		entered(ConsoleUp{PIN: cs.PIN, Leave: cs.Leave})
	}
	noConsole, _ := cmd.Flags().GetBool("no-console")
	stdin, _ := cmd.InOrStdin().(*os.File)
	if !console.Interactive(noConsole, os.Stdout, stdin) {
		if cs.Work == nil {
			return nil
		}
		return cs.Work(ctx, cmd.OutOrStdout())
	}

	client := controlClient(cmd)
	if cs.MemberID == "" && cs.PIN != "" {
		if s, err := session.Load(cs.PIN); err == nil {
			cs.MemberID = s.MemberID
		}
	}
	var livePIN atomic.Value
	livePIN.Store(cs.PIN)
	currentPIN := func() string {
		v, _ := livePIN.Load().(string)
		return v
	}
	awaitPIN := make(chan string, 1)
	if cs.PIN != "" {
		awaitPIN <- cs.PIN
	}
	workPath := func() string {
		if p := cs.Announce.WorkPath(); p != "" {
			return p
		}
		return cs.WorkPath
	}
	var announce <-chan console.MemberMsg
	if cs.Announce != nil {
		out := make(chan console.MemberMsg, 8)
		go func() {
			defer close(out)
			for msg := range cs.Announce.ch {
				if msg.PIN != "" {
					livePIN.Store(msg.PIN)
					select {
					case awaitPIN <- msg.PIN:
					default:
					}
				}
				out <- msg
			}
		}()
		announce = out
	}
	return console.Run(ctx, console.Options{
		PIN: cs.PIN, Me: cs.Me, MemberID: cs.MemberID, Branch: cs.Branch, WorkPath: cs.WorkPath,
		Elector: cs.Elector, Quit: cs.Leave, Announce: announce,
		Decide: func(ctx context.Context, id, decision string) error {
			_, err := client.DecideMember(ctx, currentPIN(), id, decision)
			return err
		},
		SetAccess: func(ctx context.Context, access string) error {
			return client.SetAccess(ctx, currentPIN(), access)
		},
	}, console.Feed{
		Control: client, PIN: cs.PIN, AwaitPIN: awaitPIN,
		LogsURL: func(ctx context.Context) (string, error) { return logsEndpoint(ctx, client, currentPIN()) },
		DevURL:  func(ctx context.Context) (string, error) { return devEndpoint(ctx, client, currentPIN()) },
		Box: func(ctx context.Context, sess controlplane.Session) (console.BoxMsg, error) {
			pin := currentPIN()
			return console.BoxMsg{
				Git: reach.ProbeSessionService(ctx, client, sess, pin, reach.ServiceGit),
				Dev: reach.ProbeSessionService(ctx, client, sess, pin, reach.ServiceDev),
			}, nil
		},
		Behind: func(ctx context.Context) (int, error) { return BehindMain(ctx, currentPIN(), workPath()) },
	}, cs.Work)
}

func BehindMain(ctx context.Context, pin, work string) (int, error) {
	main := mainRepoFor(pin)
	if work == "" || main == "" {
		return 0, nil
	}
	if _, err := os.Stat(filepath.Join(work, ".git")); err != nil {
		return 0, nil
	}
	return syncengine.CommitsBehindMain(ctx, work, main)
}

func mainRepoFor(pin string) string {
	p := session.ForPin(pin)
	bare := filepath.Join(p.Canonical, canonical.BareDir)
	if _, err := os.Stat(bare); err == nil {
		return bare
	}
	if _, err := os.Stat(p.Mirror); err == nil {
		return p.Mirror
	}
	return ""
}

func logsEndpoint(ctx context.Context, client *controlplane.Client, pin string) (string, error) {
	if client == nil {
		return "", nil
	}
	url, err := client.EndpointURL(ctx, pin, controlplane.EndpointLogs)
	var missing *controlplane.NoEndpointError
	if errors.As(err, &missing) {
		return "", nil
	}
	return url, err
}

func devEndpoint(ctx context.Context, client *controlplane.Client, pin string) (string, error) {
	if client == nil {
		return "", nil
	}
	url, err := client.EndpointURL(ctx, pin, controlplane.EndpointDev)
	var missing *controlplane.NoEndpointError
	if errors.As(err, &missing) {
		return "", nil
	}
	return url, err
}

func workPathFor(pin string) string { return session.ForPin(pin).Work }
