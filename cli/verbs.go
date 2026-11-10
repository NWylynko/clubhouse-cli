package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nwylynko/clubhouse-cli/box"
	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/conductor"
	"github.com/nwylynko/clubhouse-cli/console"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/cutover"
	"github.com/nwylynko/clubhouse-cli/devserver"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/harness"
	"github.com/nwylynko/clubhouse-cli/hoststart"
	"github.com/nwylynko/clubhouse-cli/joindaemon"
	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/monitor"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/nwylynko/clubhouse-cli/syncengine"
	"github.com/nwylynko/clubhouse-cli/telemetry"
	"github.com/nwylynko/clubhouse-cli/trust"
	"github.com/spf13/cobra"
)

func sessionCtx(cmd *cobra.Command) (session.Session, session.Paths, error) {
	pin := resolvePin(cmd)
	if pin == "" {
		return session.Session{}, session.Paths{}, fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set CLUB_PIN")
	}
	s, err := session.Load(pin)
	if err != nil {
		return session.Session{}, session.Paths{}, fmt.Errorf("load session %s: %w", pin, err)
	}
	return s, session.ForPin(pin), nil
}

func resolvePin(cmd *cobra.Command) string {
	if cmd != nil {
		if pin, _ := cmd.Flags().GetString("pin"); pin != "" {
			return pin
		}
	}
	if pin := os.Getenv("CLUB_PIN"); pin != "" {
		return pin
	}
	return pinFromCwd()
}

func pinFromCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return pinUnderSessions(cwd, filepath.Join(session.Home(), "sessions"))
}

func pinUnderSessions(dir, sessionsRoot string) string {
	dir = resolvePath(dir)
	sessionsRoot = resolvePath(sessionsRoot)
	rel, err := filepath.Rel(sessionsRoot, dir)
	if err != nil {
		return ""
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
		return rel[:i]
	}
	return rel
}

func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func workDir(p session.Paths) string {
	if _, err := os.Stat(filepath.Join(p.Work, ".git")); err == nil {
		return p.Work
	}
	if st, err := os.Stat(p.Canonical); err == nil && st.IsDir() {
		cand := filepath.Join(p.Canonical, canonical.WorkDir)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return p.Work
}

func mirrorDir(p session.Paths) string {
	if st, err := os.Stat(p.Mirror); err == nil && st.IsDir() {
		return p.Mirror
	}
	return ""
}

func addPinFlag(cmd *cobra.Command) {
	cmd.Flags().String("pin", "", "session PIN (or $CLUB_PIN)")
}

func addIntentFlag(cmd *cobra.Command) {
	cmd.Flags().String("intent", "", "what changed and why (required for push/sync)")
}

func runHost(cmd *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pin, _ := cmd.Flags().GetString("pin")
	seed, _ := cmd.Flags().GetString("seed")
	seedURL, _ := cmd.Flags().GetString("seed-url")
	cond, _ := cmd.Flags().GetString("conductor")
	modelStr, _ := cmd.Flags().GetString("model")
	once, _ := cmd.Flags().GetBool("once")
	serveOnly, _ := cmd.Flags().GetBool("serve-only")
	devCmd, _ := cmd.Flags().GetString("dev")
	installCmd, _ := cmd.Flags().GetString("install")
	takeover, _ := cmd.Flags().GetBool("takeover")

	if err := checkSeedFlags(seed, seedURL); err != nil {
		return err
	}

	plan, wizard, err := resolveFirstRun(cmd, seedOrURL(seed, seedURL))
	if err != nil {
		return err
	}
	if plan.BoxTarget != "" {
		if err := preflightBox(ctx, cmd, plan.BoxTarget, cmd.OutOrStdout()); err != nil {
			return err
		}
	}
	warnMirrorCredential(cmd, plan)

	if plan.Box || plan.BoxTarget != "" {
		return standUpOnBox(ctx, cmd, plan, once)
	}

	startInstall, startDev := strings.Fields(installCmd), strings.Fields(devCmd)
	if wizard {
		startInstall, startDev = nil, nil
	}
	ann, lv := newAnnouncer(), &leaver{}
	defer func() { _ = lv.Leave(context.Background()) }()

	work := func(ctx context.Context, out io.Writer) error {
		r, err := hoststart.Start(ctx, hoststart.Options{
			PIN: pin, SeedDir: seed, SeedURL: seedURL,
			Conductor: cond, Model: modelStr, Brief: plan.Brief,
			ServeOnly:      serveOnly,
			BoxBoot:        cmd.Name() == "_host",
			Agents:         agentsOption(plan),
			Brains:         testBrains(),
			DevCommand:     startDev,
			InstallCommand: startInstall,
			Control:        controlClient(cmd),
			Takeover:       takeover,
			MirrorURL:      plan.MirrorURL,
		})
		if err != nil {
			return err
		}
		pin = r.PIN
		lv.holds(func() { _ = r.Close(context.Background()) })
		ann.Announce(console.MemberMsg{PIN: r.PIN, MemberID: r.MemberID, Branch: r.Branch, WorkPath: r.WorkDir})
		fmt.Fprint(out, r.PrintJoin())

		if wizard {
			scaffoldNow(ctx, cmd, out, r.Host, r.PIN, plan)
			plan = recordRunCommands(ctx, cmd, out, r.Host, r.PIN, plan)
			startLocalDev(ctx, cmd, r, plan)
		}
		if once {
			return nil
		}

		log := logx.New("host")
		mode := "conductor"
		if serveOnly {
			mode = "serve-only (elected conductor merges off-box)"
		}
		log.Infof("session %s live — mode=%s", r.PIN, mode)
		log.Infof("git=%s  control=%s", r.GitURL, r.ControlURL)
		if r.WorkDir != "" && r.WorkDir != r.Host.Work {
			log.Infof("edit here: %s (branch %s) — then `club sync`", r.WorkDir, r.Branch)
			log.Infof("demo mirror (tracks main, do not edit): %s", r.Host.Work)
		} else {
			log.Infof("work=%s", r.Host.Work)
		}
		log.Infof("ticking every 2s — Ctrl-C to stop (CLUB_LOG=debug for git traces)")

		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		tick := 0
		for {
			select {
			case <-ctx.Done():
				log.Infof("stopping")
				fmt.Fprintln(out, "host stopping")
				return nil
			case <-r.Demoted():
				log.Infof("this machine is no longer the host for %s — stopping the fleet", r.PIN)
				fmt.Fprintln(out, "no longer the host — hosting moved to another machine; run `club join "+r.PIN+"` to follow it")
				return nil
			case <-t.C:
				tick++
				if r.Placement != nil {
					if err := r.Placement.Tick(ctx); err != nil {
						log.Debugf("tick %d: placement: %v", tick, err)
					}
				}
				old, err := sbGit.Output(ctx, r.Host.Work, "rev-parse", "HEAD")
				if err != nil {
					log.Warnf("tick %d: read work HEAD: %v", tick, err)
				}
				old = strings.TrimSpace(old)

				if !serveOnly {
					if ahead, err := r.Host.BranchesAheadOfMain(ctx); err != nil {
						log.Warnf("tick %d: list branches ahead: %v", tick, err)
					} else if len(ahead) > 0 {
						log.Infof("tick %d: %d branch(es) ahead of main: %s", tick, len(ahead), strings.Join(ahead, ", "))
					} else {
						log.Debugf("tick %d: nothing ahead of main", tick)
					}
					if err := r.Fleet.TickRoles(ctx); err != nil {
						log.Warnf("tick %d: fleet tick: %v", tick, err)
					}
				} else {
					log.Debugf("tick %d: serve-only, skipping fleet", tick)
				}

				if err := r.Host.SyncWorkToMain(ctx); err != nil {
					log.Warnf("tick %d: sync work to main: %v", tick, err)
				}
				newH, err := sbGit.Output(ctx, r.Host.Work, "rev-parse", "HEAD")
				if err != nil {
					log.Warnf("tick %d: read new work HEAD: %v", tick, err)
				}
				newH = strings.TrimSpace(newH)
				advanced := old != "" && newH != "" && old != newH
				if advanced {
					log.Infof("main advanced %s → %s — reinstalling deps / reconciling dev server", short(old), short(newH))
				}

				if err := r.AdoptDeclaredRun(ctx); err != nil {
					log.Warnf("tick %d: adopt declared run commands: %v", tick, err)
				}

				if err := devserver.PostMergeInstall(ctx, r.Host.Work, old, newH, r.InstallCommand, sbGit.Output,
					r.Dev.Logs.Writer(devserver.StreamStderr, devserver.PhaseInstall)); err != nil {
					log.Warnf("tick %d: post-merge install: %v", tick, err)
				}
				if r.Runtime != nil {
					if _, err := r.Runtime.ReconcileFromTo(ctx, old, newH); err != nil {
						log.Warnf("tick %d: runtime reconcile: %v", tick, err)
					}
				}
				r.KeepDevAlive(ctx, advanced)
			}
		}
	}
	finish := func(err error) error {
		var joinAs *hoststart.ErrJoinAsClient
		if !errors.As(err, &joinAs) {
			return err
		}
		fmt.Fprintln(cmd.ErrOrStderr(), joinAs.Message)
		if devCmd != "" {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: joining as a client, so --dev %q will NOT run here — the dev server belongs to whoever hosts canonical\n", devCmd)
		}
		if installCmd != "" {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: joining as a client, so --install %q will NOT run here — installs belong to whoever hosts canonical\n", installCmd)
		}
		joinPIN := pin
		if joinAs.Session.PIN != "" {
			joinPIN = joinAs.Session.PIN
		}
		return runJoinAs(cmd, joinPIN, "host")
	}

	if once {
		return finish(work(ctx, cmd.OutOrStdout()))
	}

	return finish(runConsole(ctx, cmd, consoleSession{
		PIN: pin, Me: clientName(),
		Elector:  !serveOnly,
		Leave:    lv.Leave,
		Work:     work,
		Announce: ann,
	}))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func runJoin(cmd *cobra.Command, args []string) error {
	pin := args[0]
	name, _ := cmd.Flags().GetString("name")
	return runJoinAs(cmd, pin, name)
}

func runJoinAs(cmd *cobra.Command, pin, name string) error {
	if name == "" {
		name = clientName()
	}
	client := controlClient(cmd)
	log := logx.New("join")
	log.Infof("joining %s via control plane %s", pin, client.Base)
	ctx := context.Background()
	if err := checkOrphanedCanonical(ctx, client, pin); err != nil {
		return err
	}
	j, err := joindaemon.Join(ctx, joindaemon.Options{
		PIN: pin, Name: name, Control: client,
	})
	if err != nil {
		log.Errorf("join failed: %v", err)
		return err
	}
	log.Infof("joined %s as %s", pin, j.Session.Branch)
	log.Infof("remote=%s overlay=%s", j.Remote, j.Session.HostOverlayAddr)
	log.Infof("work=%s", j.Paths.Work)
	log.Infof("mirror refreshing every 2s — Ctrl-C to stop (CLUB_LOG=debug for git traces)")
	fmt.Fprintf(cmd.OutOrStdout(), "joined %s as %s\nwork: %s\n", pin, j.Session.Branch, j.Paths.Work)
	fmt.Fprintln(cmd.OutOrStdout(), "mirror daemon running — Ctrl-C to stop")

	lv := &leaver{}
	lv.holds(func() { log.Infof("stopping"); j.Close() })
	defer func() { _ = lv.Leave(ctx) }()
	return runConsole(ctx, cmd, consoleSession{
		PIN: pin, Me: j.Name, MemberID: j.MemberID, Branch: j.Session.Branch, WorkPath: j.Paths.Work,
		Leave: lv.Leave,
		Work: func(ctx context.Context, _ io.Writer) error {
			waitForSignal(ctx)
			return nil
		},
	})
}

func waitForSignal(ctx context.Context) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(ch)
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

func checkOrphanedCanonical(ctx context.Context, client *controlplane.Client, pin string) error {
	p := session.ForPin(pin)
	if _, err := os.Stat(filepath.Join(p.Canonical, canonical.BareDir)); err != nil {
		return nil
	}
	sess, err := client.Session(ctx, pin)
	if err == nil {
		// raw endpoint ok: presence check only — is anybody serving canonical.
		if ep, ok := sess.Endpoints[controlplane.EndpointGit]; ok && ep.URL != "" {
			return nil
		}
	}
	if errors.Is(err, controlplane.ErrUpgradeRequired) {
		return controlplane.ErrUpgradeRequired
	}
	return fmt.Errorf("session %s: this machine holds canonical at %s, but the control plane %s shows no live host serving it — "+
		"there is nothing to join. Re-host it onto a box with `club box add <user@host> --pin %s`, "+
		"which takes the session over and serves this canonical again",
		pin, p.Canonical, client.Base, pin)
}

func runSync(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	intent, _ := cmd.Flags().GetString("intent")
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	if s.Branch == "" {
		id = sbGit.SessionIdentity("agent", s.PIN)
	}
	branch := s.Branch
	if branch == "" {
		branch = "client/local"
	}
	ctx := context.Background()
	followHost(ctx, s, p, id)
	base, err := syncengine.Sync(ctx, workDir(p), branch, "origin", mirrorDir(p), intent, id)
	if report := base.Report(); report != "" {
		fmt.Fprintln(cmd.OutOrStdout(), "note: "+report)
	}
	if err != nil {
		return err
	}
	syncengine.AnnouncePushed(ctx, controlClient(cmd), s.PIN, clientName(), branch, workDir(p), intent)
	fmt.Fprintln(cmd.OutOrStdout(), "ok sync")
	return nil
}

func runPush(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	intent, _ := cmd.Flags().GetString("intent")
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	branch := s.Branch
	if branch == "" {
		branch = "client/local"
	}
	ctx := context.Background()
	followHost(ctx, s, p, id)
	if err := syncengine.Push(ctx, workDir(p), branch, "origin", intent, id); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok push")
	return nil
}

func runPull(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	ctx := context.Background()
	followHost(ctx, s, p, id)
	base, err := syncengine.Pull(ctx, workDir(p), "origin", mirrorDir(p), "", id)
	if report := base.Report(); report != "" {
		fmt.Fprintln(cmd.OutOrStdout(), "note: "+report)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok pull")
	return nil
}

func followHost(ctx context.Context, s session.Session, p session.Paths, id sbGit.Identity) {
	client := controlClient(nil)
	_, _ = syncengine.FollowHost(ctx, syncengine.FollowOpts{
		Work: workDir(p), Mirror: mirrorDir(p), PIN: s.PIN,
		Cursors: p.Cursors, ID: id,
		Resolve: func(ctx context.Context, pin string) (string, int, error) {
			return client.GitURL(ctx, pin)
		},
	})
}

var controlClients sync.Map

func controlClient(_ *cobra.Command) *controlplane.Client {
	url := controlplane.BaseURL(controlFlag)
	if v, ok := controlClients.Load(url); ok {
		return v.(*controlplane.Client)
	}
	c := controlplane.NewClient(url)
	actual, _ := controlClients.LoadOrStore(url, c)
	return actual.(*controlplane.Client)
}

func runRepoint(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	to, _ := cmd.Flags().GetString("to")
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	ctx := context.Background()
	work, mirror := workDir(p), mirrorDir(p)
	gen := 0

	if to == "" {
		client := controlClient(cmd)
		var err error
		to, gen, err = client.GitURL(ctx, s.PIN)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", s.PIN, err)
		}
	}
	cur, _ := syncengine.OriginURL(ctx, work)
	if err := syncengine.Repoint(ctx, work, mirror, to, id); err != nil {
		return err
	}
	_ = syncengine.SaveCursors(p.Cursors, syncengine.Cursors{Endpoint: to, Generation: gen})
	if cur == to || strings.TrimRight(cur, "/") == strings.TrimRight(to, "/") {
		fmt.Fprintln(cmd.OutOrStdout(), "ok repoint (unchanged)")
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "ok repoint\n  %s\n→ %s\n", cur, to)
	}
	return nil
}

func runRun(cmd *cobra.Command, args []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	_ = s
	return devserver.Run(context.Background(), workDir(p), args, cmd.OutOrStdout())
}

func runConductor(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	remote, _ := cmd.Flags().GetString("remote")
	localCanonical := false
	if remote == "" {
		if _, err := os.Stat(filepath.Join(p.Canonical, "bare.git")); err == nil {
			localCanonical = true
		} else if url := resolveCanonicalRemote(ctx, s.PIN); url != "" {
			remote = url
		}
	}

	var host *canonical.Host
	if remote != "" && !localCanonical {
		published := remote
		client := controlClient(cmd)
		if sess, serr := client.Session(ctx, s.PIN); serr != nil {
			return fmt.Errorf("resolve session %s for remote %s: %w", s.PIN, published, serr)
		} else if dialed, derr := client.Dialable(ctx, sess, published); derr != nil {
			return canonical.ExplainRemoteOpenFailure(s.PIN, published, derr)
		} else {
			remote = dialed
		}
		host, err = canonical.OpenRemote(ctx, filepath.Join(p.Root, "replica"), s.PIN, remote)
		if err != nil {
			return canonical.ExplainRemoteOpenFailure(s.PIN, published, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "conductor driving remote canonical: %s\n", published)
	} else {
		host, err = canonical.Open(p.Canonical, s.PIN)
		if err != nil {
			return err
		}
	}
	defer host.Close(context.Background())

	log := logx.New("conductor")
	built := conductor.BuildSessionFleet(ctx, conductor.SessionFleetSpec{
		Host: host, ID: sbGit.SessionIdentity("conductor", s.PIN),
		Control: controlClient(cmd), PIN: s.PIN,
		Fallback: harness.FirstAvailable(),
	})
	fleet := built.Fleet
	if built.LogsURL != "" {
		if err := (&conductor.RemoteLogSource{URL: built.LogsURL}).Probe(); err == nil {
			log.Infof("error-watcher watching dev-server logs at %s (reachable)", built.LogsURL)
		} else {
			log.Warnf("error-watcher cannot read dev-server logs at %s: %v — it will see nothing until that answers", built.LogsURL, err)
		}
	} else {
		log.Infof("error-watcher idle — no HTTP dev-server logs to watch (remote=%q)", host.RemoteURL())
	}
	if built.HasIntelligence() {
		log.Infof("conducting session %s — %s", s.PIN, built.Summary())
	} else {
		log.Infof("conducting session %s (no harness — mechanical incoming-bias resolution)", s.PIN)
	}

	gate := conductor.NewRefreshGate(controlClient(cmd), s.PIN, host.RemoteURL() != "")
	refresh := func() error {
		if gate.Due(time.Now()) {
			return host.Refresh(ctx)
		}
		return nil
	}

	once, _ := cmd.Flags().GetBool("once")
	if once {
		if err := refresh(); err != nil {
			return err
		}
		return fleet.TickAll(ctx)
	}
	tick := func() error {
		if err := refresh(); err != nil {
			return err
		}
		if err := fleet.TickRoles(ctx); err != nil {
			return err
		}
		return fleet.TickAfter(ctx)
	}
	controlClient(cmd).Watch(ctx, s.PIN)
	go built.Publisher.Run(ctx)
	fmt.Fprintln(cmd.OutOrStdout(), "conductor running — Ctrl-C to stop")
	return tickUntilDone(ctx, log, tick)
}

func tickUntilDone(ctx context.Context, log *logx.Logger, tick func() error) error {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Infof("stopping")
			return nil
		case <-t.C:
			if err := tick(); err != nil {
				log.Warnf("tick: %v", err)
			}
		}
	}
}

func conductorLoop(ctx context.Context, cmd *cobra.Command, pin, remote string) error {
	conductorCmd := &cobra.Command{}
	conductorCmd.SetContext(ctx)
	conductorCmd.Flags().String("pin", pin, "")
	conductorCmd.Flags().String("remote", remote, "")
	conductorCmd.Flags().Bool("once", false, "")
	conductorCmd.SetOut(cmd.OutOrStdout())
	conductorCmd.SetErr(cmd.ErrOrStderr())
	return runConductor(conductorCmd, nil)
}

func checkSeedFlags(seedDir, seedURL string) error {
	if seedDir != "" && seedURL != "" {
		return fmt.Errorf("--seed %s and --seed-url %s both name where the session starts from — pass one, not both",
			seedDir, seedURL)
	}
	if seedDir == "" {
		return nil
	}
	return canonical.PreflightSeedDir(seedDir)
}

func ResolveSeedURL(ctx context.Context, pin, target, container string) string {
	s, err := controlClient(nil).Session(ctx, pin)
	if err != nil {
		return ""
	}
	// raw endpoint ok: the seed URL is handed back to the box, which resolves
	ep, ok := s.Endpoints[controlplane.EndpointGit]
	if !ok || ep.URL == "" {
		return ""
	}
	if s.Box != nil && s.Box.Target == target && s.Box.Container == container {
		return ""
	}
	return ep.URL
}

func resolveCanonicalRemote(ctx context.Context, pin string) string {
	s, err := controlClient(nil).Session(ctx, pin)
	if err != nil {
		return ""
	}
	// raw endpoint ok: the seed URL is handed back to the box, which resolves
	ep, ok := s.Endpoints[controlplane.EndpointGit]
	if !ok {
		return ""
	}
	return ep.URL
}

func runDaemon(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	refresh := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		followHost(ctx, s, p, id)
		return sbGit.Run(ctx, p.Mirror, "fetch", "origin", "+refs/heads/main:refs/heads/main")
	}
	once, _ := cmd.Flags().GetBool("once")
	if once {
		if err := refresh(); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "ok mirror")
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintln(cmd.OutOrStdout(), "mirror daemon running — Ctrl-C to stop")
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_ = refresh()
		}
	}
}

func runMonitor(cmd *cobra.Command, _ []string) error {
	once, _ := cmd.Flags().GetBool("once")
	asJSON, _ := cmd.Flags().GetBool("json")
	localOnly, _ := cmd.Flags().GetBool("local")
	interval, _ := cmd.Flags().GetDuration("interval")
	if interval <= 0 {
		interval = 2 * time.Second
	}
	home := session.Home()
	client := controlClient(cmd)
	out := cmd.OutOrStdout()

	var pin string
	if asJSON && !localOnly {
		pin = resolvePin(cmd)
		if pin == "" {
			return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set CLUB_PIN")
		}
	}

	frame := func() {
		ctx, cancel := context.WithTimeout(context.Background(), interval)
		defer cancel()
		if asJSON && !localOnly {
			sess, err := client.Session(ctx, pin)
			if err != nil {
				fmt.Fprintf(out, `{"error":%q}`+"\n", err.Error())
				return
			}
			_ = jsonEncode(out, sess)
			return
		}
		base := ""
		if !localOnly {
			base = client.Base
		}
		all := monitor.Snapshot(ctx, home, base)
		header := fmt.Sprintf("club monitor · home=%s · control=%s", home, dashStr(base))
		monitor.Render(out, header, all)
	}

	if once {
		frame()
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fmt.Fprint(out, "\033[2J\033[H")
		frame()
		fmt.Fprintln(out, "\n(refreshing every", interval, "— Ctrl-C to stop)")
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func jsonEncode(w interface{ Write([]byte) (int, error) }, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func dashStr(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func boxRunnerFn(cmd *cobra.Command, args []string) (box.Runner, error) {
	if fake := testHooks().BoxRunner; fake != nil {
		return fake(cmd, args)
	}
	return boxRunner(cmd, args)
}

func boxRunner(cmd *cobra.Command, args []string) (box.Runner, error) {
	if local, _ := cmd.Flags().GetBool("local"); local {
		return box.LocalRunner{}, nil
	}
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return nil, fmt.Errorf("pass an ssh target (user@host) or --local")
	}
	if strings.EqualFold(args[0], "local") {
		return box.LocalRunner{}, nil
	}
	return &box.SSHRunner{Addr: args[0]}, nil
}

func boxRunnerForTarget(cmd *cobra.Command, target string) (box.Runner, error) {
	return boxRunnerFn(cmd, []string{target})
}

func boxContainer(cmd *cobra.Command, pin string) string {
	if n, _ := cmd.Flags().GetString("name"); n != "" {
		return n
	}
	return "clubhouse-" + pin
}

func BoxAddOptions(cmd *cobra.Command, pin string) (box.Options, error) {
	warn := func(format string, a ...any) {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: "+format+"\n", a...)
	}
	buildLocal, _ := cmd.Flags().GetBool("build-local")
	image, _ := cmd.Flags().GetString("image")

	var binary []byte
	binPath, _ := cmd.Flags().GetString("binary")
	if binPath != "" && !buildLocal {
		warn("--binary is only used with --build-local; ignoring on the pull path")
		binPath = ""
	}
	if binPath != "" {
		b, err := os.ReadFile(binPath)
		if err != nil {
			return box.Options{}, fmt.Errorf("read --binary: %w", err)
		}
		binary = b
	}
	if buildLocal && len(binary) == 0 {
		return box.Options{}, fmt.Errorf("--build-local requires --binary <path to linux/amd64 club>")
	}

	seedURL, _ := cmd.Flags().GetString("seed-url")
	serveOnly, _ := cmd.Flags().GetBool("serve-only")
	volume, _ := cmd.Flags().GetString("volume")
	devCmd, _ := cmd.Flags().GetString("dev")
	installCmd, _ := cmd.Flags().GetString("install")
	briefText, _ := cmd.Flags().GetString("brief")
	pullPolicy, _ := cmd.Flags().GetString("pull")
	requireMatch, _ := cmd.Flags().GetBool("require-version-match")
	noCutover, _ := cmd.Flags().GetBool("no-cutover")
	control := controlClient(cmd)
	containerControl := strings.TrimSpace(os.Getenv("CLUB_BOX_CONTROL"))
	if containerControl == "" {
		containerControl = "http://clubhouse-control-plane:7777"
	}

	if noCutover && (devCmd != "" || installCmd != "") {
		var dropped []string
		if installCmd != "" {
			dropped = append(dropped, fmt.Sprintf("--install %q", installCmd))
		}
		if devCmd != "" {
			dropped = append(dropped, fmt.Sprintf("--dev %q", devCmd))
		}
		warn("--no-cutover leaves hosting where it is, so the box joins as a client and %s will not run there.\n"+
			"         drop --no-cutover to have the box host + install deps + supervise the dev server, or start it yourself with `club box run`",
			strings.Join(dropped, " and "))
	}

	opt := box.Options{
		Container:           boxContainer(cmd, pin),
		Binary:              binary,
		PIN:                 pin,
		SeedURL:             seedURL,
		ServeOnly:           serveOnly,
		Volume:              volume,
		Dev:                 devCmd,
		Install:             installCmd,
		Brief:               briefText,
		Takeover:            !noCutover,
		BuildLocal:          buildLocal,
		PullPolicy:          pullPolicy,
		CLIVersion:          Version,
		RequireVersionMatch: requireMatch,
		ControlURL:          control.Base,
		ContainerControlURL: containerControl,
		Control:             control,
		Telemetry:           byoBoxTelemetry(),
	}
	if buildLocal {
		opt.Image = image
		if cmd.Flags().Changed("pull") {
			warn("--pull has no effect with --build-local (nothing is pulled)")
		}
		return opt, nil
	}
	if image == "" {
		image = box.DefaultImageRef(Version)
	}
	opt.ImageRef = image
	return opt, nil
}

func runBoxAdd(cmd *cobra.Command, args []string) error {
	r, err := boxRunnerFn(cmd, args)
	if err != nil {
		return err
	}
	pin := resolvePin(cmd)
	if pin == "" {
		return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin")
	}
	opt, err := BoxAddOptions(cmd, pin)
	if err != nil {
		return err
	}
	drain, _ := cmd.Flags().GetBool("drain")
	noCutover, _ := cmd.Flags().GetBool("no-cutover")

	seedDir, _ := cmd.Flags().GetString("seed")
	seedURL, _ := cmd.Flags().GetString("seed-url")
	if err := checkSeedFlags(seedDir, seedURL); err != nil {
		return err
	}

	b, err := provisionAndCutover(context.Background(), cmd, r, pin, opt, seedDir, drain, noCutover)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "new joins:\n\n    club join %s\n\n", b.JoinPIN)
	fmt.Fprintf(out, "git:     %s\ncontrol: %s\n\n", b.GitURL, controlClient(cmd).Base)
	printDevTail(cmd, args, pin, opt.Dev, b)
	return nil
}

func provisionAndCutover(ctx context.Context, cmd *cobra.Command, r box.Runner, pin string, opt box.Options, seedDir string, drain, noCutover bool) (*box.Box, error) {
	if opt.SeedURL == "" {
		opt.SeedURL = ResolveSeedURL(ctx, pin, r.Target(), opt.Container)
		switch {
		case opt.SeedURL != "":
			fmt.Fprintf(cmd.ErrOrStderr(), "seeding box canonical from %s\n", opt.SeedURL)
		case resolveCanonicalRemote(ctx, pin) != "":
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: the session's only canonical is the container being replaced — the new box starts blank.\n"+
				"         pass --seed-url <url>, or --volume <path> to persist canonical across re-provisions\n")
		}
	}
	seedURL := opt.SeedURL
	if opt.MemberSecret == "" {
		inv, err := controlClient(cmd).Invite(ctx, pin, controlplane.MemberJoinRequest{
			Name: "box", Role: controlplane.RoleBox,
		})
		if err != nil {
			return nil, fmt.Errorf("invite the box: %w", err)
		}
		opt.MemberID, opt.MemberSecret = inv.MemberID, inv.Secret
	}
	b, err := box.Provision(ctx, r, opt)
	if err != nil {
		return nil, err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "box provisioned on %s (container %s)\n\n", b.Target, b.Container)

	if seedDir != "" {
		if err := canonical.SeedRemote(ctx, b.GitURL, seedDir, sbGit.SessionIdentity("host", pin)); err != nil {
			return nil, fmt.Errorf("seed the box's canonical from %s: %w", seedDir, err)
		}
		fmt.Fprintf(out, "seeded canonical from %s\n\n", seedDir)
	}

	if !noCutover {
		from := seedURL
		client := controlClient(cmd)
		logsURL := ""
		if b.GitURL != "" {
			// raw endpoint ok: republished verbatim through the cutover — the
			if sess, err := client.Session(ctx, pin); err == nil {
				// raw endpoint ok: printed for a human, alongside the dev URL above.
				if ep, ok := sess.Endpoints[controlplane.EndpointLogs]; ok {
					logsURL = ep.URL
				}
			}
		}
		res, err := cutover.Flip(ctx, cutover.Options{
			PIN: pin, NewDialAddr: b.GitURL, FromURL: from, Drain: drain,
			Control: client, LogsURL: logsURL, Generation: b.Generation,
		})
		if err != nil {
			return nil, fmt.Errorf("cutover: %w", err)
		}
		_ = client.PutBox(ctx, pin, controlplane.BoxFacts{
			Target: b.Target, Container: b.Container, Image: opt.ImageRef, Version: Version,
		})
		fmt.Fprintf(out, "cutover: control plane flipped → %s (gen %d)", res.DialAddr, res.Generation)
		if res.CaughtUp {
			fmt.Fprint(out, " (main caught up)")
		}
		if res.Drained {
			fmt.Fprint(out, " [drain]")
		}
		fmt.Fprint(out, "\nexisting clients follow on next `club sync` — no re-join\n\n")
	}
	return b, nil
}

func printDevTail(cmd *cobra.Command, args []string, pin, devCmd string, b *box.Box) {
	out := cmd.OutOrStdout()
	if devCmd == "" {
		fmt.Fprintf(out, "start the dev server on the box:\n\n    club box run -- <dev command>\n")
		return
	}
	client := controlClient(cmd)
	ctx := context.Background()
	if devURL, err := devEndpoint(ctx, client, pin); err == nil && devURL != "" {
		fmt.Fprintf(out, "dev:     %s\n", devURL)
		return
	}
	sess, _ := client.Session(ctx, pin)
	logsURL := ""
	// raw endpoint ok: printed for a human, alongside the dev URL above.
	if ep, ok := sess.Endpoints[controlplane.EndpointLogs]; ok {
		logsURL = ep.URL
	}
	if logsURL == "" {
		logsURL = conductor.LogsURLFromRemote(b.GitURL)
	}
	fmt.Fprintf(out, "dev:     %q supervised on the box — logs: %s\n", devCmd, dashStr(logsURL))
	fmt.Fprintf(out, "         its URL appears once the container registers with the control plane\n")
	fmt.Fprintf(out, "         watch it with:  club box logs\n")
}

func runElect(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	revoke, _ := cmd.Flags().GetBool("revoke")
	self, _ := cmd.Flags().GetBool("self")
	box, _ := cmd.Flags().GetString("box")
	name, _ := cmd.Flags().GetString("name")
	hName, _ := cmd.Flags().GetString("harness")
	model, _ := cmd.Flags().GetString("model")
	if name == "" {
		name = s.Branch
	}
	if name == "" {
		name = os.Getenv("USER")
	}
	if name == "" {
		name = "agent"
	}
	if hName == "" {
		if c := harness.FirstAvailable(); c != nil {
			hName = string(c.Name)
		} else {
			hName = "claude"
		}
	}

	if revoke {
		st, err := trust.LoadPersist(p.Root)
		if err != nil {
			return fmt.Errorf("no active election to revoke: %w", err)
		}
		if st.BoxRoot != "" {
			_ = trust.DeactivateOnBox(st.BoxRoot)
			_ = trust.WipeBox(st.BoxRoot)
		}
		_ = trust.ClearPersist(p.Root)
		fmt.Fprintln(cmd.OutOrStdout(), "revoked harness-runner election — stop conductor AI on this laptop")
		return nil
	}

	if self {
		b := trust.NewBroker(trust.RemoteBox)
		if err := b.SelfElect(name); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), b.LastWarning())
		_ = trust.ClearPersist(p.Root)
		return nil
	}

	sess, err := controlClient(cmd).Session(context.Background(), s.PIN)
	if err != nil {
		return err
	}
	kind := trust.LocalHost
	if sess.Box != nil {
		kind = trust.RemoteBox
	}
	b := trust.NewBroker(kind)
	fmt.Fprintln(cmd.OutOrStdout(), b.WarnBeforeElect(name, hName))

	lease, err := b.Elect(trust.ElectRequest{Elector: name, Harness: hName, Model: model})
	if err != nil {
		_ = b.Close()
		return err
	}
	if box != "" {
		if _, err := trust.InstallOnBox(box, lease); err != nil {
			_ = b.Close()
			return err
		}
	}
	_ = trust.SavePersist(p.Root, trust.PersistState{
		PIN: s.PIN, Elector: lease.Elector, Harness: lease.Harness, Model: lease.Model,
		RunnerID: lease.RunnerID, BoxRoot: box,
	})
	_ = controlClient(cmd).PutConductor(context.Background(), s.PIN, controlplane.ConductorRecord{
		Elector: lease.Elector, Harness: lease.Harness, Model: lease.Model,
		RunnerID: lease.RunnerID, Active: true,
	})
	fmt.Fprintf(cmd.OutOrStdout(),
		"elected %s (%s)\nrunner id: %s\nAI roles run on THIS laptop via the %s harness — login stays here.\nNow run `club conductor` here (this session is %s — export CLUB_PIN=%s to skip\n--pin on off-box commands). It resolves the box canonical from the control plane\nand drives merges off-box (or pass --remote <box-git-url>).\n",
		lease.Elector, lease.Harness, lease.RunnerID, lease.Harness, s.PIN, s.PIN)
	_ = b.Close()
	return nil
}

func byoBoxTelemetry() string {
	if on, _ := telemetry.Resolve(); on {
		return telemetry.ModeOn
	}
	return telemetry.ModeOff
}
