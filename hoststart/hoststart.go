package hoststart

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nwylynko/clubhouse-cli/admission"
	"github.com/nwylynko/clubhouse-cli/boxexec"
	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/conductor"
	"github.com/nwylynko/clubhouse-cli/contracts"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/detect"
	"github.com/nwylynko/clubhouse-cli/devserver"
	"github.com/nwylynko/clubhouse-cli/durability"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/gitserver"
	"github.com/nwylynko/clubhouse-cli/harness"
	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/netbind"
	"github.com/nwylynko/clubhouse-cli/placement"
	"github.com/nwylynko/clubhouse-cli/runfile"
	"github.com/nwylynko/clubhouse-cli/runtime"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/nwylynko/clubhouse-cli/sessionnet"
	"github.com/nwylynko/clubhouse-cli/syncengine"
	"github.com/nwylynko/clubhouse-cli/telemetry"
)

type ErrJoinAsClient struct {
	Message string
	Session controlplane.Session
}

func (e *ErrJoinAsClient) Error() string { return e.Message }

type Options struct {
	PIN            string
	Name           string
	SeedDir        string
	SeedURL        string
	Conductor      string
	Model          string
	Brief          string
	Brain          *harness.Client
	Agents         map[string]controlplane.RoleAgent
	Brains         map[string]*harness.Client
	Control        *controlplane.Client
	DevCommand     []string
	InstallCommand []string
	ServeOnly      bool
	Takeover       bool
	MirrorURL      string
	BoxBoot        bool
}

const (
	roleMerger  = "merger"
	roleWatcher = "error-watcher"
	roleSetup   = "setup"
)

var fleetRoles = []string{roleMerger, roleWatcher, roleSetup}

func resolveBrains(opt Options, log *logx.Logger) map[string]*harness.Client {
	shared := opt.Brain
	if shared == nil {
		shared = harness.Lookup(opt.Conductor, opt.Model)
	}
	out := make(map[string]*harness.Client, len(fleetRoles))
	for _, role := range fleetRoles {
		if c, ok := opt.Brains[role]; ok {
			out[role] = c
			continue
		}
		a, ok := opt.Agents[role]
		if !ok || a.Harness == "" {
			out[role] = shared
			continue
		}
		if !harness.IsAvailable(a.Harness) {
			log.Warnf("role %s asked for %s, which is not installed here — running it mechanically. "+
				"Install %s (or elect a machine that has it) to restore the session's choice.", role, a.Harness, a.Harness)
			out[role] = nil
			continue
		}
		out[role] = harness.Lookup(a.Harness, a.Model)
	}
	return out
}

func harnessName(c *harness.Client) string {
	if c == nil {
		return ""
	}
	return string(c.Name)
}

func publishAgents(ctx context.Context, client *controlplane.Client, pin string, opt Options) {
	roles := map[string]controlplane.RoleAgent{}
	for _, role := range fleetRoles {
		switch a, ok := opt.Agents[role]; {
		case ok && a.Harness != "":
			roles[role] = a
		case opt.Conductor != "":
			roles[role] = controlplane.RoleAgent{Harness: opt.Conductor, Model: opt.Model}
		}
	}
	if len(roles) == 0 || client == nil {
		return
	}
	primary := roles[roleMerger]
	_ = client.PutConductor(ctx, pin, controlplane.ConductorRecord{
		Harness: primary.Harness, Model: primary.Model, Roles: roles,
	})
}

type Running struct {
	PIN               string
	Host              *canonical.Host
	Fleet             *conductor.Fleet
	Runtime           *runtime.Reconciler
	Dev               *devserver.Supervisor
	Control           *controlplane.Client
	GitURL            string
	GitDirect         string
	DemoURL           string
	ControlURL        string
	MemberID          string
	WorkDir           string
	Branch            string
	Machine           string
	InstallCommand    []string
	Placement         *placement.Loop
	sessionNet        *gitserver.SessionNet
	bind              string
	devHolder         *devserver.Holder
	devHolderM        sync.Mutex
	execHolder        *boxexec.Holder
	execHolderM       sync.Mutex
	gen               atomic.Int64
	logs              *devserver.LogBuffer
	log               *logx.Logger
	stopHB            chan struct{}
	demoted           chan struct{}
	stopReason        atomic.Value
	Outbox            *controlplane.Outbox
	stoppedGitPublish atomic.Bool
}

func (r *Running) StopReason() string {
	s, _ := r.stopReason.Load().(string)
	return s
}

func (r *Running) stopHosting(reason string) {
	select {
	case <-r.demoted:
		return
	default:
	}
	r.stopReason.Store(reason)
	close(r.demoted)
}

func (r *Running) Generation() int { return int(r.gen.Load()) }

func Start(ctx context.Context, opt Options) (r *Running, err error) {
	pin := opt.PIN
	handed := pin != ""
	if handed {
		if err := controlplane.ValidatePIN(pin); err != nil {
			return nil, err
		}
	}

	log := logx.New("host")
	var localGen int
	if handed {
		localGen = syncengine.LoadCursors(session.ForPin(pin).Cursors).Generation
	}

	client := opt.Control
	if client == nil {
		client = controlplane.NewClient(controlplane.BaseURL(""))
	}
	boxMemberID := ""

	log.Infof("control plane %s — connecting", client.Base)
	hCtx, hCancel := context.WithTimeout(ctx, 10*time.Second)
	health, herr := client.Health(hCtx)
	hCancel()
	if herr != nil {
		return nil, fmt.Errorf("control plane %s unreachable: %w", client.Base, herr)
	}
	log.Infof("control plane ok — %s (db %s)", client.Base, health.DB)

	if secret := strings.TrimSpace(os.Getenv("CLUB_MEMBER_SECRET")); secret != "" {
		if !handed {
			return nil, fmt.Errorf("CLUB_MEMBER_SECRET set but no PIN was handed — box boot requires --pin")
		}
		id := strings.TrimSpace(os.Getenv("CLUB_MEMBER_ID"))
		if err := session.WriteMembership(pin, id, secret); err != nil {
			return nil, fmt.Errorf("plant invited membership: %w", err)
		}
		client.RememberMembership(pin, id, secret)
		boxMemberID = id

		recordFromBootOnwards(ctx, client, pin, id, log)

		defer func() {
			if err != nil {
				reportBoxBootFailure(ctx, client, pin, boxMemberID, err, log)
			}
		}()
	}

	bind := netbind.BindForControl(client.Base)
	remoteControl := !netbind.LoopbackURL(client.Base)
	switch {
	case bind == "auto":
		ip, _ := netbind.AdvertiseHostMode("auto")
		log.Infof("control plane is remote — direct session dials will use %s; plain git stays on loopback", ip)
	case bind == "" && remoteControl:
		_, err := netbind.RoutableIP()
		log.Warnf("control plane is remote but this machine has no routable address (%v) — no direct dial; clients reach git via the relay", err)
	}

	hostname, _ := os.Hostname()
	stack := detect.Probe().DefaultStack()
	name := defaultMemberName(opt)
	memberRole := controlplane.RoleHost
	if opt.ServeOnly {
		memberRole = controlplane.RoleBox
	}
	overlay := ""
	if pin != "" {
		overlay = "club-" + pin + ".host"
	}
	if opt.Takeover && pin != "" && opt.SeedURL == "" && !canonicalExists(session.ForPin(pin).Canonical) {
		if _, err := seedFromLiveReplica(ctx, client, pin, session.ReadMemberID(pin),
			session.ForPin(pin).Canonical, "before taking over", log); err != nil {
			log.Errorf("canonical setup failed for %s: %v", pin, err)
			return nil, err
		}
	}

	claim, err := client.Claim(ctx, controlplane.ClaimRequest{
		PIN: pin, HostMachine: hostname, LocalGeneration: localGen,
		OverlayAddr: overlay,
		Stack:       &controlplane.StackInfo{Runtime: stack.Runtime, Version: stack.Version, PkgMgr: stack.PkgMgr},
		Harness:     opt.Conductor, SeedURL: opt.SeedURL,
		Takeover: opt.Takeover, MemberName: name, MemberRole: memberRole,
	})
	if err != nil {
		return nil, fmt.Errorf("control plane claim: %w", err)
	}
	pin = claim.Session.PIN
	if err := controlplane.ValidatePIN(pin); err != nil {
		return nil, fmt.Errorf("control plane returned an invalid pin %q: %w", pin, err)
	}
	paths := session.ForPin(pin)
	if err := os.MkdirAll(paths.Root, 0o755); err != nil {
		return nil, err
	}
	standup, err := session.BeginStandup(pin, session.RoleHost, "")
	if err != nil {
		return nil, err
	}
	defer standup.Release()
	if claim.JoinOnly {
		return nil, &ErrJoinAsClient{Message: claim.Message, Session: claim.Session}
	}
	if claim.Takeover {
		log.Infof("took over hosting %s at generation %d — the previous host is now demoted", pin, claim.Session.Generation)
	}
	if claim.Created {
		log.Infof("session minted as %s", pin)
	}

	var host *canonical.Host
	switch {
	case canonicalExists(paths.Canonical):
		log.Infof("resuming existing canonical for %s at %s", pin, paths.Canonical)
		host, err = canonical.Open(paths.Canonical, pin)
		if err == nil {
			err = ensureWork(ctx, host)
		}
	case opt.SeedURL != "":
		log.Infof("seeding canonical for %s from %s", pin, opt.SeedURL)
		host, err = seedFromRemote(ctx, paths.Canonical, pin, opt.SeedURL)
	default:
		host, err = seedFromLiveReplica(ctx, client, pin, claim.MemberID, paths.Canonical, "", log)
		if err != nil {
			return nil, err
		}
		if host == nil {
			log.Infof("creating fresh canonical for %s at %s", pin, paths.Canonical)
			host, err = canonical.Create(ctx, paths.Canonical, pin)
		}
	}
	if err != nil {
		log.Errorf("canonical setup failed for %s: %v", pin, err)
		return nil, err
	}
	if opt.SeedDir != "" {
		if err := seedFromDir(ctx, host, opt.SeedDir); err != nil {
			_ = host.Close(ctx)
			return nil, err
		}
	}
	if err := requireHistory(ctx, host); err != nil {
		_ = host.Close(ctx)
		return nil, err
	}
	host.Bind = bind
	var sessNet *gitserver.SessionNet
	if sn, err := sessionNetFor(ctx, claim.Session, bind, client.HolderTicket(claim.Session.PIN, "git")); err != nil {
		log.Warnf("not joining the session network: %v — clients must reach %s directly", err, bind)
	} else if sn != nil {
		host.Session = sn
		sessNet = sn
	}
	gitURL, err := host.StartServer()
	if err != nil && opt.Takeover && sessNet != nil && errors.Is(err, sessionnet.ErrHolderBusy) {
		gitURL, err = waitForIncumbentToStandDown(ctx, log, host, time.Duration(controlplane.DefaultLeaseTTL)*time.Second)
	}
	if err != nil {
		_ = host.Close(ctx)
		return nil, fmt.Errorf("serve session git for %s: %w", pin, err)
	}
	log.Infof("serving session git at %s", gitURL)
	if sessionURL := host.SessionRemoteURL(); sessionURL != "" {
		log.Infof("session git also reachable at %s (session network — no direct route needed)", sessionURL)
		gitURL = sessionURL
	}
	if remoteControl && netbind.LoopbackURL(gitURL) {
		log.Errorf("git endpoint %s is loopback but the control plane %s is remote: clients on other machines will fail to clone. The plain listener is always loopback — this session needs a relay (BindForControl publishes a direct address when something routable exists).", gitURL, client.Base)
	}

	if opt.Brief != "" {
		if err := conductor.WriteBrief(ctx, host, sbGit.SessionIdentity("host", pin), opt.Brief); err != nil {
			log.Warnf("could not record the brief on main (the setup role has nothing to act on): %v", err)
		} else {
			log.Infof("brief recorded on main: %s", opt.Brief)
		}
	}

	if err := conductor.WriteContracts(ctx, host, sbGit.SessionIdentity("host", pin)); err != nil {
		log.Warnf("could not record the agent contracts on main (clients still get them locally): %v", err)
	}

	if err := host.SyncWorkToMain(ctx); err != nil {
		log.Warnf("could not refresh the demo mirror after recording session files: %v", err)
	}

	gen := claim.Session.Generation
	_ = syncengine.SaveCursors(paths.Cursors, syncengine.Cursors{Endpoint: gitURL, Generation: gen})

	mirrorURL := opt.MirrorURL
	if mirrorURL == "" {
		if sess, err := client.Session(ctx, pin); err == nil {
			// raw endpoint ok: mirror is a GitHub (or similar) HTTPS URL published
			if ep, ok := sess.Endpoints[controlplane.EndpointMirror]; ok {
				mirrorURL = ep.URL
			}
		}
	}

	if err := announceEndpoints(ctx, client, pin, gen, gitURL, host.Srv.SessionDirect(), mirrorURL); err != nil {
		if errors.Is(err, controlplane.ErrDemoted) {
			_ = host.Close(ctx)
			return nil, &ErrJoinAsClient{
				Message: fmt.Sprintf("session %s moved while announcing — joining as a client", pin),
			}
		}
		log.Warnf("announce endpoints: %v", err)
	}

	branch := "client/" + name
	if err := ensureClientTree(ctx, host, gitURL, paths, branch, name, pin, gen); err != nil {
		log.Warnf("client dev tree setup failed (host still serves; edit canonical/work as a fallback): %v", err)
		branch = ""
	} else {
		log.Debugf("client dev tree ready at %s on %s", paths.Work, branch)
	}

	meta := session.Session{
		PIN:             pin,
		Role:            session.RoleHost,
		Branch:          branch,
		HostOverlayAddr: "club-" + pin + ".host",
		Capability:      detect.Probe(),
	}
	if err := meta.Save(); err != nil {
		_ = host.Close(ctx)
		return nil, err
	}
	if err := standup.Ready(meta); err != nil {
		log.Warnf("could not mark %s ready: %v", pin, err)
	}
	workTree := host.Work
	if branch != "" {
		workTree = paths.Work
	}

	logs := &devserver.LogBuffer{}
	if host.Srv != nil {
		host.Srv.Logs = logs.String
		host.Srv.LogsSelect = func(stream, phase string) string {
			return logs.Select(devserver.Stream(stream), devserver.Phase(phase))
		}
		host.Srv.LogsSince = logs.Since
		host.Srv.LogsSubscribe = logs.Subscribe
	}
	declared := runfile.ReadFromMain(ctx, host)
	installCmd := runfile.Resolve(opt.InstallCommand, declared.Install, nil)
	devCmd := runfile.Resolve(opt.DevCommand, declared.Dev, nil)
	if len(opt.DevCommand) == 0 && len(declared.Dev) > 0 {
		log.Infof("dev command from the project itself (%s): %s", runfile.Path, strings.Join(devCmd, " "))
	} else if len(opt.DevCommand) > 0 || len(opt.InstallCommand) > 0 {
		if err := runfile.Commit(ctx, host, sbGit.SessionIdentity("host", pin), installCmd, devCmd); err != nil {
			log.Warnf("could not record %s (a future host will re-detect): %v", runfile.Path, err)
		}
	}
	dev := &devserver.Supervisor{WorkDir: host.Work, Logs: logs}

	id := sbGit.SessionIdentity("host", pin)
	brains := resolveBrains(opt, log)
	brain := brains[roleMerger]
	var hName string
	if brain != nil {
		hName = string(brain.Name)
	}
	mirror := &durability.Mirror{Bare: host.Bare, Config: durability.LoadConfig(mirrorURL)}
	if mirror.Enabled() {
		log.Infof("durability mirror enabled → %s", mirror.Config.RemoteURL)
	}
	merger := &conductor.Merger{Host: host, ID: id, Resolve: conductor.HarnessResolver(brains[roleMerger]), Harness: harnessName(brains[roleMerger]), Control: client, PIN: pin, Mirror: mirror}
	rt := &runtime.Reconciler{WorkDir: host.Work, Dev: dev, Control: client, PIN: pin, Generation: gen}
	watcher := &conductor.ErrorWatcher{Host: host, Logs: logs, ID: id, Fix: conductor.HarnessFixer(brains[roleWatcher]), Harness: harnessName(brains[roleWatcher]), Control: client, PIN: pin,
		Health: conductor.HTTPHealthDynamic(rt.DevURL)}
	setup := &conductor.Setup{
		Host: host, ID: id, Agent: conductor.HarnessScaffolder(brains[roleSetup]),
		Harness: harnessName(brains[roleSetup]), Control: client, PIN: pin,
	}
	publishAgents(ctx, client, pin, opt)
	fleet := &conductor.Fleet{
		Roles:    []conductor.Role{merger, watcher},
		Detached: []conductor.Role{setup},
		After:    []conductor.Role{rt},
	}

	role := memberRole
	memberID := claim.MemberID
	if memberID == "" {
		var err error
		memberID, err = client.JoinMember(ctx, pin, controlplane.MemberJoinRequest{
			Name: name, Role: role, Branch: branch, Machine: hostname,
			Harness: hName, Capability: controlplane.CapFromProfile(detect.Probe()),
		})
		if err != nil {
			log.Warnf("member join: %v", err)
		}
	} else {
		if err := client.Heartbeat(ctx, pin, memberID, controlplane.MemberUpdate{
			Branch: branch, Harness: hName, Role: role,
			Capability: controlplane.CapFromProfile(detect.Probe()),
		}); err != nil {
			log.Warnf("member update after claim: %v", err)
		}
	}

	r = &Running{
		PIN: pin, Host: host, Fleet: fleet, Runtime: rt, Dev: dev,
		Control: client, GitURL: gitURL, GitDirect: host.Srv.SessionDirect(),
		DemoURL:    "file://" + host.Work,
		ControlURL: client.Base, MemberID: memberID,
		WorkDir: workTree, Branch: branch, Machine: hostname,
		InstallCommand: installCmd,
		logs:           logs,
		log:            log,
		stopHB:         make(chan struct{}), demoted: make(chan struct{}),
		sessionNet: sessNet, bind: bind,
	}
	r.gen.Store(int64(gen))

	if memberID != "" {
		meta.MemberID = memberID
		_ = meta.Save()
	}
	r.Placement = &placement.Loop{
		Control: client, PIN: pin, MemberID: memberID, Name: name, Machine: hostname,
		Stop: func(ctx context.Context, service string) error {
			if service == controlplane.ServiceDev {
				r.stopDevHolder()
			}
			if service == controlplane.ServiceGit {
				r.stopGitHolder()
			}
			return nil
		},
		OnChange: func(service, detail string) { log.Infof("placement: %s %s", service, detail) },
		Start: func(ctx context.Context, service string) error {
			if service == controlplane.ServiceGit {
				return r.startGitHolder()
			}
			return nil
		},
		Serves: func(service string) bool {
			switch service {
			case controlplane.ServiceGit:
				return true
			case controlplane.ServiceDev:
				return r.Dev != nil && len(r.Dev.Command) > 0
			case controlplane.ServiceConductor:
				return !opt.ServeOnly
			}
			return false
		},
	}
	r.claimServedLeases(ctx, opt)

	if len(installCmd) > 0 || len(devCmd) > 0 {
		if err := r.StartDev(ctx, installCmd, devCmd); err != nil {
			_ = host.Close(ctx)
			return nil, err
		}
	}
	r.Control.Watch(ctx, pin)
	if r.MemberID != "" {
		r.Outbox = controlplane.NewOutbox(r.Control, pin, r.MemberID)
		r.Control.RegisterOutbox(pin, r.Outbox)
		if r.Placement != nil {
			r.Placement.Outbox = r.Outbox
		}
	}
	go r.heartbeat(ctx, logs)
	if opt.BoxBoot {
		r.startExecHolder(ctx, log)
	}
	return r, nil
}

func (r *Running) AdoptDeclaredRun(ctx context.Context) error {
	if r.Dev != nil && len(r.Dev.Command) > 0 {
		return nil
	}
	declared := runfile.ReadFromMain(ctx, r.Host)
	if len(declared.Dev) == 0 {
		return nil
	}
	log := r.log
	if log == nil {
		log = logx.New("host")
	}
	log.Infof("the project now declares its own run commands (%s) — installing and supervising: %s",
		runfile.Path, strings.Join(declared.Dev, " "))
	return r.StartDev(ctx, runfile.Resolve(r.InstallCommand, declared.Install, nil), declared.Dev)
}

func (r *Running) StartDev(ctx context.Context, install, dev []string) error {
	log := r.log
	if log == nil {
		log = logx.New("host")
	}
	if len(install) > 0 || len(dev) > 0 {
		cmd := install
		if len(cmd) == 0 {
			cmd = devserver.DetectInstall(r.Host.Work)
		}
		if len(cmd) > 0 {
			log.Infof("installing deps: %s (in %s)", strings.Join(cmd, " "), r.Host.Work)
			if err := devserver.Install(ctx, r.Host.Work, cmd,
				r.logs.Writer(devserver.StreamStderr, devserver.PhaseInstall)); err != nil {
				if ctx.Err() != nil {
					return fmt.Errorf("install interrupted: %w", ctx.Err())
				}
				log.Warnf("install failed (continuing): %v", err)
			}
		}
		r.InstallCommand = cmd
	}
	if len(dev) == 0 {
		return nil
	}
	r.Dev.Command = dev
	if err := r.Dev.Start(ctx); err != nil {
		log.Warnf("dev server did not start (continuing): %v", err)
		return nil
	}
	log.Infof("dev server started: %s (in %s)", strings.Join(dev, " "), r.Host.Work)
	go watchDevStartup(ctx, r.Dev, dev, r.logs, log)
	r.startDevHolder(ctx, log)
	devPort, _ := runtime.LocalDevPort(r.Host.Work)
	if err := session.PublishDevURL(r.PIN, fmt.Sprintf("http://127.0.0.1:%d", devPort)); err != nil {
		log.Warnf("could not publish the dev URL for `club site`: %v", err)
	}
	if r.Runtime != nil {
		r.Runtime.AnnounceDev(ctx)
	}
	return nil
}

func (r *Running) KeepDevAlive(ctx context.Context, mainAdvanced bool) {
	if r.Dev == nil {
		return
	}
	if mainAdvanced {
		r.Dev.ClearGiveUp()
	}
	if r.Dev.Running() && r.Runtime != nil {
		r.Runtime.AnnounceDev(ctx)
	}
	if !r.Dev.NeedsRestart() {
		return
	}
	log := r.log
	if log == nil {
		log = logx.New("host")
	}
	command := strings.Join(r.Dev.Command, " ")
	lasted := r.Dev.RanFor().Round(time.Millisecond)
	err := r.Dev.Restart(ctx)
	switch {
	case errors.Is(err, devserver.ErrGaveUp):
		msg := fmt.Sprintf("dev server %q exited immediately %d times in a row — not restarting it again. "+
			"Fix the command (or the project) and merge; the next commit on main gives it another go. Its last output:\n%s",
			command, r.Dev.ShortExits(), r.logTail(20))
		log.Errorf("%s", msg)
		r.writeDevLog("club: " + msg)
	case err != nil:
		log.Warnf("dev server %q could not be restarted: %v", command, err)
	default:
		msg := fmt.Sprintf("dev server %q had exited after %s — restarted it (attempt %d)",
			command, lasted, r.Dev.ShortExits())
		log.Infof("%s", msg)
		r.writeDevLog("club: " + msg)
		if r.Runtime != nil {
			r.Runtime.AnnounceDev(ctx)
		}
		go watchDevStartup(ctx, r.Dev, r.Dev.Command, r.devLogs(), log)
	}
}

func (r *Running) writeDevLog(line string) {
	if b := r.devLogs(); b != nil {
		b.WriteLine(devserver.StreamClub, devserver.PhaseDev, line)
	}
}

func (r *Running) devLogs() *devserver.LogBuffer {
	if r.logs != nil {
		return r.logs
	}
	if r.Dev != nil {
		return r.Dev.Logs
	}
	return nil
}

func (r *Running) logTail(n int) string {
	if b := r.devLogs(); b != nil {
		return b.Tail(n)
	}
	return ""
}

func watchDevStartup(ctx context.Context, dev *devserver.Supervisor, command []string, logs *devserver.LogBuffer, log *logx.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-dev.Done():
	}
	err := dev.Err()
	if err == nil {
		return
	}
	log.Warnf("dev server %q exited after %s: %v — nothing will answer on its port. Its last output:\n%s",
		strings.Join(command, " "), dev.RanFor().Round(time.Millisecond), err, logs.Tail(20))
}

func announceEndpoints(ctx context.Context, client *controlplane.Client, pin string, gen int, gitURL, gitDirect, mirrorURL string) error {
	gh, gp := splitURL(gitURL)
	if err := client.PutEndpoint(ctx, pin, controlplane.EndpointGit, controlplane.EndpointPut{
		URL: gitURL, Host: gh, Port: gp, Direct: gitDirect, Source: "host", Generation: gen,
	}); err != nil {
		return err
	}
	logsURL := logsURLFromGit(gitURL)
	if logsURL != "" {
		if err := client.PutEndpoint(ctx, pin, controlplane.EndpointLogs, controlplane.EndpointPut{
			URL: logsURL, Generation: gen, Source: "host",
		}); err != nil {
			return err
		}
	}
	if cfg := durability.LoadConfig(mirrorURL); cfg.RemoteURL != "" {
		if err := client.PutEndpoint(ctx, pin, controlplane.EndpointMirror, controlplane.EndpointPut{
			URL: cfg.RemoteURL, Generation: gen, Source: "host",
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Running) heartbeat(ctx context.Context, logs *devserver.LogBuffer) {
	t := time.NewTicker(controlplane.MemberCycle)
	defer t.Stop()
	log := logx.New("host")
	admissionLog := &admission.Log{Control: r.Control, PIN: r.PIN}
	updates := r.Control.Updates(r.PIN)
	for {
		select {
		case <-r.stopHB:
			return
		case <-ctx.Done():
			return
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			admissionLog.Stream(u.Events)
		case <-t.C:
			if !r.Control.StreamLive(r.PIN) {
				admissionLog.Poll(ctx)
			}
			r.Control.FlushBuffer(context.Background())
			hbCtx, cancel := context.WithTimeout(context.Background(), controlplane.MemberCycle)
			err := r.cycle(hbCtx)
			cancel()
			if errors.Is(err, controlplane.ErrDemoted) {
				if gen, ok := r.adoptCutoverOntoSelf(context.Background()); ok {
					log.Infof("generation advanced to %d by a cutover that left us hosting — continuing as host", gen)
					continue
				}
				log.Warnf("demoted: control plane rejected stale generation %d — stopping host role", r.Generation())
				r.stopHosting("moved")
				return
			}
			if errors.Is(err, controlplane.ErrNoSession) || errors.Is(err, controlplane.ErrUnauthorized) {
				log.Warnf("session %s ended — stopping host role (%v); canonical is still at %s",
					r.PIN, err, r.Host.Root)
				r.stopHosting("expired")
				return
			}
			if err != nil {
				log.Warnf("member cycle: %v", err)
			} else {
				log.Infof("member cycle — git endpoint %s (gen %d)", r.GitURL, r.Generation())
			}
			_ = logs
		}
	}
}

func (r *Running) cycle(ctx context.Context) error {
	if r.Outbox == nil {
		if r.Placement == nil || r.Placement.HoldsService(controlplane.ServiceGit) {
			err := r.Control.PutEndpoint(ctx, r.PIN, controlplane.EndpointGit, controlplane.EndpointPut{
				URL: r.GitURL, Direct: r.GitDirect, Generation: r.Generation(), Source: "host-heartbeat",
			})
			if err != nil {
				return err
			}
			r.stoppedGitPublish.Store(false)
		} else if !r.stoppedGitPublish.Swap(true) {
			logx.New("host").Infof("%s: no longer hold git — stopping endpoint publication", r.PIN)
		}
		if r.MemberID != "" {
			_ = r.Control.Heartbeat(ctx, r.PIN, r.MemberID, controlplane.MemberUpdate{})
		}
		return nil
	}
	r.Outbox.SetGeneration(r.Generation())
	r.Outbox.SetMember(controlplane.MemberUpdate{})
	if r.Placement == nil || r.Placement.HoldsService(controlplane.ServiceGit) {
		r.Outbox.SetEndpoint(controlplane.EndpointGit, controlplane.EndpointPut{
			URL: r.GitURL, Direct: r.GitDirect, Generation: r.Generation(), Source: "host-heartbeat",
		})
		r.stoppedGitPublish.Store(false)
	} else if !r.stoppedGitPublish.Swap(true) {
		logx.New("host").Infof("%s: no longer hold git — stopping endpoint publication", r.PIN)
	}
	if r.Placement != nil {
		for _, svc := range controlplane.Services {
			if r.Placement.HoldsService(svc) {
				r.Outbox.Hold(svc)
			}
		}
	}
	res, err := r.Outbox.Flush(ctx)
	if err != nil {
		return err
	}
	if res.Conflict {
		return controlplane.ErrDemoted
	}
	if r.Placement != nil {
		r.Placement.ApplyLost(ctx, res.Lost)
	}
	return nil
}

func (r *Running) adoptCutoverOntoSelf(ctx context.Context) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sess, err := r.Control.Session(ctx, r.PIN)
	if err != nil {
		return 0, false
	}
	if sess.Generation <= r.Generation() || sess.HostMachine != r.Machine {
		return 0, false
	}
	// raw endpoint ok: compared against the string this host itself published.
	ep, ok := sess.Endpoints[controlplane.EndpointGit]
	if !ok || !sameURL(ep.URL, r.GitURL) || ep.Source != "cutover" {
		return 0, false
	}
	r.gen.Store(int64(sess.Generation))
	if r.Runtime != nil {
		r.Runtime.SetGeneration(sess.Generation)
	}
	_ = syncengine.SaveCursors(session.ForPin(r.PIN).Cursors, syncengine.Cursors{
		Endpoint: r.GitURL, Generation: sess.Generation,
	})
	return sess.Generation, true
}

func sameURL(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

func (r *Running) Demoted() <-chan struct{} { return r.demoted }

func ensureClientTree(ctx context.Context, host *canonical.Host, gitURL string, paths session.Paths, branch, name, pin string, gen int) error {
	id := sbGit.SessionIdentity(name, pin)
	if _, err := os.Stat(filepath.Join(paths.Work, ".git")); err == nil {
		return contracts.Install(paths.Work, pin)
	}
	_ = host.CreateClientBranch(ctx, branch)
	if err := syncengine.CloneClient(ctx, host.Bare, paths.Work, branch, id); err != nil {
		return err
	}
	if err := contracts.Install(paths.Work, pin); err != nil {
		return err
	}
	_ = syncengine.SaveCursors(paths.Cursors, syncengine.Cursors{Endpoint: gitURL, Generation: gen})
	return nil
}

func (r *Running) claimServedLeases(ctx context.Context, opt Options) {
	services := []string{controlplane.ServiceGit}
	if len(r.Dev.Command) > 0 {
		services = append(services, controlplane.ServiceDev)
	}
	if !opt.ServeOnly {
		services = append(services, controlplane.ServiceConductor)
	}
	for _, svc := range services {
		if err := r.Placement.Adopt(ctx, svc); err != nil {
			logx.New("host").Debugf("claim %s lease: %v", svc, err)
		}
	}
}

func (r *Running) Close(ctx context.Context) error {
	select {
	case <-r.stopHB:
	default:
		close(r.stopHB)
	}
	if r.Control != nil {
		r.Control.CloseWatch(r.PIN)
		r.Control.UnregisterOutbox(r.PIN)
	}
	if r.Placement != nil {
		r.Placement.ReleaseAll(context.Background())
	}
	if r.MemberID != "" && r.Control != nil {
		_ = r.Control.LeaveMember(context.Background(), r.PIN, r.MemberID)
	}
	r.stopDevHolder()
	r.stopExecHolder()
	_ = r.Dev.Stop()
	_ = session.ClearLive(r.PIN)
	return r.Host.Close(ctx)
}

func (r *Running) startDevHolder(ctx context.Context, log *logx.Logger) {
	if r.sessionNet == nil || r.Runtime == nil {
		return
	}
	r.devHolderM.Lock()
	defer r.devHolderM.Unlock()
	if r.devHolder != nil {
		return
	}
	port, why := runtime.LocalDevPort(r.Host.Work)
	if port <= 0 {
		log.Warnf("dev server is not on the session network — %s", why)
		return
	}
	opt := devserver.HolderOptions{
		Relay: r.sessionNet.Relay, PIN: r.PIN, Key: r.sessionNet.Key, LocalPort: port,
	}
	if r.Control != nil {
		opt.Ticket = r.Control.HolderTicket(r.PIN, "dev")
	}
	if r.sessionNet.Direct {
		if dln, advHost, err := netbind.ListenAdvertise(r.bind, 0); err == nil {
			opt.DirectListener = dln
			opt.DirectAdvertise = net.JoinHostPort(advHost, strconv.Itoa(dln.Addr().(*net.TCPAddr).Port))
		} else {
			log.Debugf("no direct listener for the dev server: %v", err)
		}
	}
	h, err := devserver.StartHolder(context.WithoutCancel(ctx), opt)
	if err != nil {
		log.Warnf("dev server not published on the session network: %v", err)
		return
	}
	r.devHolder = h
	r.Runtime.SetSessionDev(h.URL(), h.Direct())
	r.Runtime.AnnounceDev(ctx)
}

func (r *Running) stopDevHolder() {
	r.devHolderM.Lock()
	h := r.devHolder
	r.devHolder = nil
	r.devHolderM.Unlock()
	if h == nil {
		return
	}
	_ = h.Close()
	if r.Runtime != nil {
		r.Runtime.SetSessionDev("", "")
	}
}

func (r *Running) stopGitHolder() {
	if r.Host == nil || r.Host.Srv == nil {
		return
	}
	r.Host.Srv.UnpublishSession()
	if strings.HasPrefix(r.GitURL, "club://") {
		r.GitURL = r.Host.RemoteURL()
	}
	r.GitDirect = ""
	log := r.log
	if log == nil {
		log = logx.New("host")
	}
	log.Infof("no longer publishing git on the session network — lease moved")
}

func (r *Running) startGitHolder() error {
	if r.sessionNet == nil || r.Host == nil || r.Host.Srv == nil {
		return nil
	}
	if err := r.Host.Srv.PublishSession(); err != nil {
		return err
	}
	if u := r.Host.SessionRemoteURL(); u != "" {
		r.GitURL = u
	}
	r.GitDirect = r.Host.Srv.SessionDirect()
	return nil
}

func (r *Running) startExecHolder(ctx context.Context, log *logx.Logger) {
	if r.sessionNet == nil || r.Host == nil {
		return
	}
	r.execHolderM.Lock()
	defer r.execHolderM.Unlock()
	if r.execHolder != nil {
		return
	}
	h, err := boxexec.StartHolder(context.WithoutCancel(ctx), boxexec.HolderOptions{
		Relay:   r.sessionNet.Relay,
		PIN:     r.PIN,
		Key:     r.sessionNet.Key,
		WorkDir: r.Host.Work,
		Ticket: func() (string, error) {
			if r.Control == nil {
				return "", nil
			}
			return r.Control.HolderTicket(r.PIN, boxexec.Service)()
		},
		OnShutdown: func() {
			r.stopHosting("box removed")
		},
	})
	if err != nil {
		log.Warnf("box exec not published on the session network: %v", err)
		return
	}
	r.execHolder = h
	log.Infof("box exec published on the session network for %s", r.PIN)
}

func (r *Running) stopExecHolder() {
	r.execHolderM.Lock()
	h := r.execHolder
	r.execHolder = nil
	r.execHolderM.Unlock()
	if h != nil {
		_ = h.Close()
	}
}

func (r *Running) PrintJoin() string {
	stack := detect.Probe().DefaultStack()
	return fmt.Sprintf("detected %s %s + %s — starting session (change?)\n\nsession live. share with your team:\n\n    club join %s\n\ndemo url:  %s\ngit:       %s\ncontrol:   %s\n",
		stack.Runtime, stack.Version, stack.PkgMgr, r.PIN, r.DemoURL, r.GitURL, r.ControlURL)
}

func NewPIN() (string, error) { return controlplane.MintPIN() }

func canonicalExists(root string) bool {
	_, err := os.Stat(filepath.Join(root, canonical.BareDir))
	return err == nil
}

func ensureWork(ctx context.Context, host *canonical.Host) error {
	if _, err := os.Stat(filepath.Join(host.Work, ".git")); err == nil {
		return nil
	}
	_ = os.RemoveAll(host.Work)
	return sbGit.Run(ctx, "", "clone", "--branch", "main", host.Bare, host.Work)
}

func seedFromDir(ctx context.Context, host *canonical.Host, dir string) error {
	id := sbGit.SessionIdentity("host", host.PIN)
	if err := canonical.PreflightSeedDir(dir); err != nil {
		return err
	}
	if err := canonical.CopyTree(dir, host.Work); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: host.Work, Env: id.EnvVars()}
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	status, _ := c.Output(ctx, "status", "--porcelain")
	if status == "" {
		return nil
	}
	if err := c.Run(ctx, "commit", "-m", "seed from "+filepath.Base(dir)); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}

func reseedSource(ctx context.Context, client *controlplane.Client, pin, self string, log *logx.Logger) (string, string, error) {
	sess, err := client.Session(ctx, pin)
	if err != nil {
		return "", "", fmt.Errorf("no canonical on disk for %s and the control plane could not say whether the session has any: %w", pin, err)
	}
	hadHistory := sess.Convergence != nil && sess.Convergence.MainSHA != ""
	var freshest *controlplane.Member
	for i := range sess.Members {
		m := &sess.Members[i]
		if m.MainMirrorHeight == 0 && m.MainMirrorSHA == "" {
			continue
		}
		hadHistory = true
		if m.ID == self || !m.Online || m.State == controlplane.MemberLeft {
			continue
		}
		if freshest == nil || m.MainMirrorHeight > freshest.MainMirrorHeight {
			freshest = m
		}
	}
	if !hadHistory {
		return "", "", nil
	}
	if freshest == nil {
		return "", "", fmt.Errorf("no canonical on disk for %s and no live member holds a replica of main — "+
			"this session's code is gone from every machine that is still here. Nothing was published: "+
			"a fresh empty canonical would look healthy and serve nobody's work", pin)
	}
	who := fmt.Sprintf("%s (height %d)", freshest.Name, freshest.MainMirrorHeight)
	url, err := client.EndpointURL(ctx, pin, controlplane.EndpointGit)
	if err != nil {
		return "", who, fmt.Errorf("no canonical on disk for %s and the session's git endpoint could not be resolved to seed from "+
			"(%s holds the freshest replica at height %d): %w", pin, freshest.Name, freshest.MainMirrorHeight, err)
	}
	log.Infof("%s: seeding from %s (freshest replica, height %d)", pin, freshest.Name, freshest.MainMirrorHeight)
	return url, who, nil
}

func recordFromBootOnwards(ctx context.Context, client *controlplane.Client, pin, memberID string, log *logx.Logger) {
	if memberID == "" {
		return
	}
	cycleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := client.MemberSync(cycleCtx, pin, memberID, controlplane.MemberSync{WantSnapshot: true}); err != nil {
		log.Warnf("%s: this box could not run its first member cycle (%v) — it is booting anyway, and until it claims "+
			"nothing it logs will reach the session's telemetry", pin, err)
	}
}

func reportBoxBootFailure(ctx context.Context, client *controlplane.Client, pin, memberID string, cause error, log *logx.Logger) {
	var joinInstead *ErrJoinAsClient
	if errors.As(cause, &joinInstead) {
		return
	}
	log.Errorf("this box cannot host %s and is stopping: %v", pin, cause)
	if memberID != "" {
		sayCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := client.ReportBoxBootFailure(sayCtx, pin, memberID, cause.Error()); err != nil {
			log.Warnf("%s: the control plane was not told why this box could not boot (%v) — its record will say "+
				"provisioning until the provisioner's own deadline", pin, err)
		}
		cancel()
	}
	telemetry.StopMember()
}

const BoxSeedWait = 8 * time.Minute

func seedFromLiveReplica(ctx context.Context, client *controlplane.Client, pin, self, root, when string, log *logx.Logger) (*canonical.Host, error) {
	seedURL, freshest, err := reseedSource(ctx, client, pin, self, log)
	if err != nil || seedURL == "" {
		return nil, err
	}
	if when != "" {
		when = " " + when
	}
	log.Infof("no canonical on disk for %s — reseeding from the session's freshest replica at %s%s", pin, seedURL, when)
	host, err := seedFromRemote(ctx, root, pin, seedURL)
	if err == nil {
		return host, nil
	}
	log.Warnf("%s: nothing is serving the session's git yet (%v) — waiting up to %s for a member to take it over",
		pin, err, BoxSeedWait)
	return waitForAGitHolderThenSeed(ctx, client, pin, self, root, freshest, err, log)
}

func waitForAGitHolderThenSeed(ctx context.Context, client *controlplane.Client, pin, self, root, freshest string, lastErr error, log *logx.Logger) (*canonical.Host, error) {
	wctx, cancel := context.WithTimeout(ctx, BoxSeedWait)
	defer cancel()
	said := ""
	for {
		select {
		case <-wctx.Done():
			return nil, fmt.Errorf("no canonical on disk for %s and nobody served the session's git within %s "+
				"(%s holds the freshest replica) — refusing to publish an empty canonical over it. Last attempt: %w",
				pin, BoxSeedWait, freshest, lastErr)
		case <-time.After(controlplane.MemberCycle):
		}
		sess, err := client.Session(wctx, pin)
		if err != nil {
			lastErr = err
			continue
		}
		holder, ok := sess.Leases[controlplane.ServiceGit]
		if !ok || holder.Owner == "" || holder.Owner == self || !holder.Live(time.Now()) {
			if said != "nobody" {
				said = "nobody"
				log.Infof("%s: still nobody holds the session's git — %s has the freshest replica", pin, freshest)
			}
			continue
		}
		seedURL, err := client.EndpointURL(wctx, pin, controlplane.EndpointGit)
		if err != nil {
			lastErr = err
			continue
		}
		who := holder.OwnerName
		if who == "" {
			who = holder.Owner
		}
		if said != who {
			said = who
			log.Infof("%s: %s holds the session's git now — seeding from %s", pin, who, seedURL)
		}
		host, err := seedFromRemote(wctx, root, pin, seedURL)
		if err == nil {
			log.Infof("%s: seeded from %s", pin, who)
			return host, nil
		}
		lastErr = err
		log.Debugf("%s: %s holds git but is not serving it yet (%v) — retrying", pin, who, err)
	}
}

func requireHistory(ctx context.Context, host *canonical.Host) error {
	c := &sbGit.Cmd{Dir: host.Bare}
	if _, err := c.Output(ctx, "rev-parse", "--verify", "main"); err != nil {
		return fmt.Errorf("canonical at %s has no history on main — refusing to serve it as the session's truth: %w", host.Bare, err)
	}
	return nil
}

func seedFromRemote(ctx context.Context, root, pin, url string) (*canonical.Host, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	bare := filepath.Join(abs, canonical.BareDir)
	work := filepath.Join(abs, canonical.WorkDir)
	if err := sbGit.Run(ctx, "", "clone", "--bare", url, bare); err != nil {
		if rmErr := os.RemoveAll(bare); rmErr != nil {
			return nil, fmt.Errorf("seed from url: %w (and the partial clone at %s could not be removed: %v)", err, bare, rmErr)
		}
		return nil, fmt.Errorf("seed from url: %w", err)
	}
	_ = sbGit.Run(ctx, bare, "config", "http.receivepack", "true")
	if err := sbGit.Run(ctx, "", "clone", "--branch", "main", bare, work); err != nil {
		_ = os.RemoveAll(work)
		if err := sbGit.Run(ctx, "", "clone", bare, work); err != nil {
			return nil, err
		}
		c := &sbGit.Cmd{Dir: work}
		_ = c.Run(ctx, "branch", "-M", "main")
		_ = c.Run(ctx, "push", "origin", "main")
	}
	return &canonical.Host{
		Root: abs, PIN: pin, Bare: bare, Work: work,
		Srv: &gitserver.Server{Bare: bare},
	}, nil
}

func splitURL(raw string) (host string, port int) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", 0
	}
	h := u.Hostname()
	p := u.Port()
	if p == "" {
		return h, 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

func sessionNetFor(ctx context.Context, sess controlplane.Session, bind string, ticket func() (string, error)) (*gitserver.SessionNet, error) {
	if sess.RelayAddr == "" {
		return nil, nil
	}
	key, err := sessionnet.ParseKey(sess.SessionKey)
	if err != nil {
		return nil, err
	}
	if key.Zero() {
		return nil, fmt.Errorf("the control plane names relay %s but minted no session key", sess.RelayAddr)
	}
	return &gitserver.SessionNet{
		Relay: sess.RelayAddr, PIN: sess.PIN, Key: key, Context: context.WithoutCancel(ctx),
		Direct: DirectIsPublishable(bind),
		Ticket: ticket,
	}, nil
}

func waitForIncumbentToStandDown(ctx context.Context, log *logx.Logger, host *canonical.Host, bound time.Duration) (string, error) {
	log.Infof("the previous git holder is still registered — waiting for it to stand down (up to %s)", bound)
	deadline := time.Now().Add(bound)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		url, err := host.StartServer()
		if err == nil {
			log.Infof("the previous git holder stood down — serving")
			return url, nil
		}
		if !errors.Is(err, sessionnet.ErrHolderBusy) || time.Now().After(deadline) {
			return "", err
		}
	}
}

func DirectIsPublishable(bind string) bool {
	if netbind.DirectSuppressed() {
		return false
	}
	host, err := netbind.AdvertiseHostMode(bind)
	if err != nil || host == "" {
		return false
	}
	return !netbind.LoopbackURL("http://" + host)
}

func logsURLFromGit(gitURL string) string {
	if sessionnet.IsSessionURL(gitURL) {
		pin, service, _, err := sessionnet.ParseURL(gitURL)
		if err != nil {
			return ""
		}
		return sessionnet.FormatURL(pin, service, "logs")
	}
	u, err := url.Parse(gitURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/logs"
}

func defaultMemberName(opt Options) string {
	if opt.Name != "" {
		return opt.Name
	}
	if opt.ServeOnly {
		return "box"
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "host"
}
