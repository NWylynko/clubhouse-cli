package joindaemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/admission"
	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/conductor"
	"github.com/nwylynko/clubhouse-cli/contracts"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/detect"
	"github.com/nwylynko/clubhouse-cli/devserver"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/harness"
	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/netbind"
	"github.com/nwylynko/clubhouse-cli/placement"
	"github.com/nwylynko/clubhouse-cli/runtime"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/nwylynko/clubhouse-cli/syncengine"
)

type Options struct {
	PIN     string
	Name    string
	Control *controlplane.Client

	ConductsHere bool
}

type Joined struct {
	Session  session.Session
	Paths    session.Paths
	Remote   string
	MemberID string
	Name     string
	Control  *controlplane.Client
	stop     chan struct{}
	log      *logx.Logger

	Placement *placement.Loop
	Outbox    *controlplane.Outbox

	conductsElsewhereInThisProcess bool

	mu          sync.Mutex
	canonical   *canonical.Host
	dev         *devserver.Supervisor
	devHolder   *devserver.Holder
	devRuntime  *runtime.Reconciler
	fleet       *conductor.Fleet
	fleetHost   *canonical.Host
	publisher   *conductor.StatePublisher
	refreshGate *conductor.RefreshGate
	fleetLogs   *conductor.RemoteLogSource
	devURL      string

	upgradeRefusal string
}

func (j *Joined) UpgradeRequired() bool { return j.UpgradeRequiredReason() != "" }

func (j *Joined) UpgradeRequiredReason() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.upgradeRefusal
}

func (j *Joined) noteUpgradeRequired(sentence string) {
	j.mu.Lock()
	first := j.upgradeRefusal == ""
	j.upgradeRefusal = sentence
	j.mu.Unlock()
	if first {
		j.log.Warnf("%s — this machine's git sync keeps working over the relay until its last session ticket expires (up to an hour), so finish what you are pushing", sentence)
	}
}

func (j *Joined) clearUpgradeRequired() {
	j.mu.Lock()
	had := j.upgradeRefusal != ""
	j.upgradeRefusal = ""
	j.mu.Unlock()
	if had {
		j.log.Infof("the control plane accepts this build again — the version refusal is over")
	}
}

func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return def
}

func Join(ctx context.Context, opt Options) (*Joined, error) {
	if err := controlplane.ValidatePIN(opt.PIN); err != nil {
		return nil, err
	}
	if err := noDaemonRunningHere(opt.PIN); err != nil {
		return nil, err
	}
	name := opt.Name
	if name == "" {
		if u := os.Getenv("USER"); u != "" {
			name = u
		} else {
			name = "agent"
		}
	}
	log := logx.New("join")
	standup, err := session.BeginStandup(opt.PIN, session.RoleClient, "client/"+name)
	if err != nil {
		return nil, err
	}
	defer standup.Release()
	client := opt.Control
	if client == nil {
		client = controlplane.NewClient(controlplane.BaseURL(""))
	}
	hostname, _ := os.Hostname()
	sess, memberID, err := resolveJoinSession(ctx, client, opt.PIN, name, hostname, log)
	if err != nil {
		return nil, err
	}
	gitURL := ""
	// raw endpoint ok: read for the log line below and for unreachableHint, which
	if ep, ok := sess.Endpoints[controlplane.EndpointGit]; ok {
		gitURL = ep.URL
	}
	published := gitURL
	if gitURL == "" {
		return nil, fmt.Errorf("session %s has no git endpoint yet (host %s has not announced one)", opt.PIN, orUnknown(sess.HostMachine))
	}
	log.Infof("control plane ok — %s resolved: git=%s generation=%d host=%s", opt.PIN, gitURL, sess.Generation, orUnknown(sess.HostMachine))
	dialable, err := client.Dialable(ctx, sess, gitURL)
	if err != nil {
		return nil, fmt.Errorf("reach %s for session %s: %w", gitURL, opt.PIN, err)
	}
	gitURL = dialable
	if hint := unreachableHint(published, client.Base); hint != "" {
		log.Warnf("%s", hint)
	}
	paths := session.ForPin(opt.PIN)
	if err := os.MkdirAll(paths.Root, 0o755); err != nil {
		return nil, err
	}
	branch := "client/" + name
	id := sbGit.SessionIdentity(name, opt.PIN)
	joined, jerr := alreadyJoinedHere(opt.PIN, paths)
	if jerr != nil {
		return nil, jerr
	}
	if joined {
		log.Infof("resuming the existing work tree at %s (branch %s)", paths.Work, branch)
		if err := syncengine.Repoint(ctx, paths.Work, paths.Mirror, gitURL, id); err != nil {
			return nil, fmt.Errorf("follow %s to its current git endpoint: %w%s", opt.PIN, err, diagnose(published))
		}
	} else {
		log.Infof("cloning work tree on branch %s from %s", branch, gitURL)
		if err := syncengine.CloneClient(ctx, gitURL, paths.Work, branch, id); err != nil {
			return nil, fmt.Errorf("clone work: %w%s", err, diagnose(published))
		}
		log.Debugf("cloning bare mirror from %s", gitURL)
		if err := sbGit.Run(ctx, "", "clone", "--bare", gitURL, paths.Mirror); err != nil {
			return nil, fmt.Errorf("clone mirror: %w%s", err, diagnose(published))
		}
	}
	if err := contracts.Install(paths.Work, opt.PIN); err != nil {
		return nil, fmt.Errorf("install contracts: %w", err)
	}
	meta := session.Session{
		PIN:             opt.PIN,
		Role:            session.RoleClient,
		Branch:          branch,
		HostOverlayAddr: sess.OverlayAddr,
		Capability:      detect.Probe(),
	}
	if err := meta.Save(); err != nil {
		return nil, err
	}
	if err := standup.Ready(meta); err != nil {
		log.Warnf("could not mark %s ready: %v", opt.PIN, err)
	}
	_ = syncengine.SaveCursors(paths.Cursors, syncengine.Cursors{
		Endpoint: gitURL, Generation: sess.Generation,
	})
	if mid, err := client.JoinMember(ctx, opt.PIN, controlplane.MemberJoinRequest{
		Name: name, Role: controlplane.RoleClient, Branch: branch, Machine: hostname,
		Capability: controlplane.CapFromProfile(detect.Probe()),
	}); err != nil {
		log.Warnf("member join: %v", err)
	} else if mid != "" {
		memberID = mid
	}
	if memberID != "" {
		meta.MemberID = memberID
		_ = meta.Save()
	}
	j := &Joined{
		Session: meta, Paths: paths, Remote: gitURL, MemberID: memberID, Name: name,
		Control: client, stop: make(chan struct{}), log: log,
	}
	j.conductsElsewhereInThisProcess = opt.ConductsHere
	j.Placement = j.newPlacementLoop()
	if opt.ConductsHere {
		if err := j.Placement.Adopt(ctx, controlplane.ServiceConductor); err != nil {
			log.Warnf("could not record that this process conducts %s (%v) — another member may take the conductor", opt.PIN, err)
		}
	}
	j.Control.Watch(context.Background(), j.Session.PIN)
	if j.MemberID != "" {
		j.Outbox = controlplane.NewOutbox(j.Control, j.Session.PIN, j.MemberID)
		j.Control.RegisterOutbox(j.Session.PIN, j.Outbox)
		if j.Placement != nil {
			j.Placement.Outbox = j.Outbox
		}
	}
	go j.mirrorLoop()
	go j.fleetLoop()
	log.Infof("mirror daemon running for %s (branch %s)", opt.PIN, branch)
	return j, nil
}

func (j *Joined) mirrorLoop() {
	t := time.NewTicker(controlplane.MemberCycle)
	defer t.Stop()
	var lastMain string
	id := sbGit.SessionIdentity(strings.TrimPrefix(j.Session.Branch, "client/"), j.Session.PIN)
	admissionLog := &admission.Log{Control: j.Control, PIN: j.Session.PIN}
	updates := j.Control.Updates(j.Session.PIN)
	mirrorFloor := time.NewTimer(controlplane.Floor(controlplane.MirrorFloor, j.Control.StreamLive(j.Session.PIN)))
	defer mirrorFloor.Stop()
	fetchNow := true
	for {
		select {
		case <-j.stop:
			return
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			for _, e := range u.Events {
				if e.Kind == "main.advanced" || e.Kind == "host.cutover" {
					fetchNow = true
				}
				if e.Kind == "host.takeover" {
					j.standDownWhatWasTaken()
				}
				if e.Kind == "host.cutover" {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					j.log.Infof("host.cutover — following new canonical")
					_, _ = syncengine.FollowHost(ctx, syncengine.FollowOpts{
						Work: j.Paths.Work, Mirror: j.Paths.Mirror, PIN: j.Session.PIN,
						Cursors: j.Paths.Cursors, ID: id,
						Resolve: func(ctx context.Context, pin string) (string, int, error) {
							return j.Control.GitURL(ctx, pin)
						},
					})
					cancel()
				}
			}
			admissionLog.Stream(u.Events)
		case <-mirrorFloor.C:
			fetchNow = true
			mirrorFloor.Reset(controlplane.Floor(controlplane.MirrorFloor, j.Control.StreamLive(j.Session.PIN)))
		case <-t.C:
			if !j.Control.StreamLive(j.Session.PIN) {
				admissionLog.Poll(context.Background())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, _ = syncengine.FollowHost(ctx, syncengine.FollowOpts{
				Work: j.Paths.Work, Mirror: j.Paths.Mirror, PIN: j.Session.PIN,
				Cursors: j.Paths.Cursors, ID: id,
				Resolve: func(ctx context.Context, pin string) (string, int, error) {
					return j.Control.GitURL(ctx, pin)
				},
			})
			if fetchNow {
				if err := sbGit.Run(ctx, j.Paths.Mirror, "fetch", "origin", "+refs/heads/main:refs/heads/main"); err != nil {
					j.log.Warnf("mirror fetch: %v", err)
				}
				fetchNow = false
				if !mirrorFloor.Stop() {
					select {
					case <-mirrorFloor.C:
					default:
					}
				}
				mirrorFloor.Reset(controlplane.Floor(controlplane.MirrorFloor, j.Control.StreamLive(j.Session.PIN)))
			}
			main, _ := sbGit.Output(ctx, j.Paths.Mirror, "rev-parse", "refs/heads/main")
			main = strings.TrimSpace(main)
			if j.Outbox != nil {
				j.Outbox.SetMember(controlplane.MemberUpdate{
					MainMirrorSHA: main, MainMirrorHeight: mirrorHeight(ctx, j.Paths.Mirror),
					Branch: j.Session.Branch, Harness: localHarness(),
					Capability: controlplane.CapFromProfile(detect.Probe()),
				})
				if j.Placement != nil {
					for _, svc := range controlplane.Services {
						if j.Placement.HoldsService(svc) {
							j.Outbox.Hold(svc)
						}
					}
				}
				res, err := j.Outbox.Flush(ctx)
				if errors.Is(err, controlplane.ErrNoSession) || errors.Is(err, controlplane.ErrUnauthorized) {
					j.log.Warnf("session %s ended — stopping join daemon (%v)", j.Session.PIN, err)
					cancel()
					j.Close()
					return
				}
				if errors.Is(err, controlplane.ErrUpgradeRequired) {
					j.noteUpgradeRequired(err.Error())
				} else if err != nil {
					j.log.Debugf("member cycle: %v", err)
				} else {
					j.clearUpgradeRequired()
					if j.Placement != nil {
						j.Placement.ApplyLost(ctx, res.Lost)
					}
				}
			} else if j.MemberID != "" {
				err := j.Control.Heartbeat(ctx, j.Session.PIN, j.MemberID, controlplane.MemberUpdate{
					MainMirrorSHA: main, MainMirrorHeight: mirrorHeight(ctx, j.Paths.Mirror),
					Branch: j.Session.Branch, Harness: localHarness(),
					Capability: controlplane.CapFromProfile(detect.Probe()),
				})
				if errors.Is(err, controlplane.ErrNoSession) || errors.Is(err, controlplane.ErrUnauthorized) {
					j.log.Warnf("session %s ended — stopping join daemon (%v)", j.Session.PIN, err)
					cancel()
					j.Close()
					return
				}
				if errors.Is(err, controlplane.ErrUpgradeRequired) {
					j.noteUpgradeRequired(err.Error())
				} else if err == nil {
					j.clearUpgradeRequired()
				}
			}
			if j.Placement != nil {
				if err := j.Placement.Tick(ctx); err != nil {
					if errors.Is(err, controlplane.ErrNoSession) || errors.Is(err, controlplane.ErrUnauthorized) {
						j.log.Warnf("session %s ended — stopping join daemon (%v)", j.Session.PIN, err)
						cancel()
						j.Close()
						return
					}
					j.log.Debugf("placement: %v", err)
				}
			}
			j.keepDevPublished(ctx)
			j.ensureDevForwarder(ctx)
			cancel()
			if main != "" && main != lastMain {
				if lastMain != "" {
					j.log.Infof("mirror: main advanced to %s (run `club sync` to integrate)", shortSHA(main))
				} else {
					j.log.Debugf("mirror: main at %s", shortSHA(main))
				}
				lastMain = main
			}
		}
	}
}

func (j *Joined) standDownWhatWasTaken() {
	if j.Outbox == nil || j.Placement == nil {
		return
	}
	held := false
	for _, svc := range controlplane.Services {
		if j.Placement.HoldsService(svc) {
			j.Outbox.Hold(svc)
			held = true
		}
	}
	if !held {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := j.Outbox.Flush(ctx)
	if err != nil {
		j.log.Debugf("stand down after takeover: %v", err)
		return
	}
	j.Placement.ApplyLost(ctx, res.Lost)
}

func (j *Joined) Fleet() *conductor.Fleet {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.fleet
}

func (j *Joined) DevURL() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.devURL
}

func (j *Joined) ensureDevForwarder(ctx context.Context) {
	j.mu.Lock()
	have := j.devURL
	j.mu.Unlock()
	if have != "" {
		return
	}
	url, err := j.Control.EndpointURL(ctx, j.Session.PIN, controlplane.EndpointDev)
	if err != nil || url == "" {
		return
	}
	j.mu.Lock()
	j.devURL = url
	j.mu.Unlock()
	if err := session.PublishDevURL(j.Session.PIN, url); err != nil {
		j.log.Warnf("could not publish the dev URL for `club site`: %v", err)
	}
	j.log.Infof("dev server: %s", url)
}

func unreachableHint(gitURL, controlBase string) string {
	if !netbind.LoopbackURL(gitURL) || netbind.LoopbackURL(controlBase) {
		return ""
	}
	return fmt.Sprintf("the host published a loopback git endpoint (%s): that address means the host's OWN machine, so it is unreachable from here. The host must use a non-loopback control plane (so it publishes a routable direct address) or a session relay.", gitURL)
}

func diagnose(gitURL string) string {
	if !netbind.LoopbackURL(gitURL) {
		return ""
	}
	return fmt.Sprintf("\nhint: %s is a loopback address — reachable only from the machine that published it. "+
		"Either the host process on THIS machine is not running, or the host is on another machine and must use a non-loopback control plane (or a session relay) so it advertises a routable address.", gitURL)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func resolveJoinSession(ctx context.Context, client *controlplane.Client, pin, name, machine string, log *logx.Logger) (controlplane.Session, string, error) {
	if session.ReadMemberSecret(pin) != "" {
		sess, err := client.Session(ctx, pin)
		if err != nil {
			if errors.Is(err, controlplane.ErrUpgradeRequired) {
				return controlplane.Session{}, "", controlplane.ErrUpgradeRequired
			}
			if errors.Is(err, controlplane.ErrUnauthorized) || errors.Is(err, controlplane.ErrNoSession) {
				if localSessionState(pin) {
					return controlplane.Session{}, "", fmt.Errorf("this session has ended")
				}
				return controlplane.Session{}, "", err
			}
			return controlplane.Session{}, "", fmt.Errorf("resolve pin %s via control plane %s: %w", pin, client.Base, err)
		}
		return sess, "", nil
	}
	reqID, err := knockID(pin)
	if err != nil {
		return controlplane.Session{}, "", err
	}
	log.Infof("knocking on %s as %s@%s", pin, name, machine)
	res, err := client.Redeem(ctx, pin, controlplane.RedeemRequest{
		Name: name, Machine: machine, RequestID: reqID,
	}, func(p controlplane.RedeemPending) {
		log.Infof("%s", admission.WaitLine(pin, p.Acceptors))
	})
	if errors.Is(err, controlplane.ErrKnockSpent) {
		log.Warnf("the earlier join request for %s was accepted while this machine was disconnected, "+
			"and its secret was not delivered — knocking again, which needs one more accept", pin)
		if err := session.ClearRequestID(pin); err != nil {
			return controlplane.Session{}, "", err
		}
		if reqID, err = knockID(pin); err != nil {
			return controlplane.Session{}, "", err
		}
		res, err = client.Redeem(ctx, pin, controlplane.RedeemRequest{
			Name: name, Machine: machine, RequestID: reqID,
		}, func(p controlplane.RedeemPending) {
			log.Infof("%s", admission.WaitLine(pin, p.Acceptors))
		})
	}
	if err != nil {
		if errors.Is(err, controlplane.ErrUpgradeRequired) {
			return controlplane.Session{}, "", controlplane.ErrUpgradeRequired
		}
		if errors.Is(err, controlplane.ErrDeclined) {
			return controlplane.Session{}, "", fmt.Errorf("%w — ask someone in the session, or try again", err)
		}
		if errors.Is(err, controlplane.ErrDoorClosed) || errors.Is(err, controlplane.ErrDoorFull) {
			return controlplane.Session{}, "", err
		}
		if errors.Is(err, controlplane.ErrNoSession) {
			if localSessionState(pin) {
				return controlplane.Session{}, "", fmt.Errorf("this session has ended")
			}
			return controlplane.Session{}, "", err
		}
		return controlplane.Session{}, "", fmt.Errorf("resolve pin %s via control plane %s: %w", pin, client.Base, err)
	}
	return res.Session, res.MemberID, nil
}

func knockID(pin string) (string, error) {
	if id := session.ReadRequestID(pin); id != "" {
		return id, nil
	}
	id, err := session.NewRequestID()
	if err != nil {
		return "", err
	}
	if err := session.WriteRequestID(pin, id); err != nil {
		return "", err
	}
	return id, nil
}

func localSessionState(pin string) bool {
	if session.ReadMemberSecret(pin) != "" {
		return true
	}
	p := session.ForPin(pin)
	if _, err := os.Stat(p.Meta); err == nil {
		return true
	}
	return isRepo(p.Work) || isRepo(p.Mirror)
}

func alreadyJoinedHere(pin string, paths session.Paths) (bool, error) {
	work, mirror := isRepo(paths.Work), isRepo(paths.Mirror)
	switch {
	case work && mirror:
		return true, nil
	case !work && !mirror:
		if entries, err := os.ReadDir(paths.Work); err == nil && len(entries) > 0 {
			return false, fmt.Errorf("%s already exists and is not a club work tree — move it aside, then join again", paths.Work)
		}
		return false, nil
	default:
		have, missing := paths.Work, paths.Mirror
		if !work {
			have, missing = paths.Mirror, paths.Work
		}
		return false, fmt.Errorf("session %s is half-present on this machine: %s is a git repo but %s is not. "+
			"Nothing here is safe to guess at — copy anything you need out of %s, remove %s, then join again",
			pin, have, missing, paths.Work, paths.Root)
	}
}

func isRepo(dir string) bool {
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		if _, err := os.Stat(filepath.Join(dir, "objects")); err == nil {
			return true
		}
	}
	return false
}

func noDaemonRunningHere(pin string) error {
	m, ok := session.LiveHere(pin)
	if !ok || m.PID == os.Getpid() {
		return nil
	}
	return fmt.Errorf("a club daemon is already running here for %s (pid %d, started %s) — "+
		"use `club open %s` for a shell in the work tree, or stop that process first",
		pin, m.PID, m.StartedAt.Local().Format("15:04:05"), pin)
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func (j *Joined) Close() {
	if j.Control != nil {
		j.Control.CloseWatch(j.Session.PIN)
		j.Control.UnregisterOutbox(j.Session.PIN)
	}
	if j.Placement != nil {
		j.Placement.ReleaseAll(context.Background())
	}
	if j.MemberID != "" && j.Control != nil {
		_ = j.Control.LeaveMember(context.Background(), j.Session.PIN, j.MemberID)
	}
	select {
	case <-j.stop:
	default:
		close(j.stop)
	}
	for _, svc := range controlplane.Services {
		_ = j.stopService(context.Background(), svc)
	}
	j.mu.Lock()
	host := j.canonical
	j.canonical = nil
	j.mu.Unlock()
	if host != nil {
		_ = host.Close(context.Background())
	}
	_ = session.ClearLive(j.Session.PIN)
}

func mirrorHeight(ctx context.Context, mirror string) int {
	out, err := sbGit.Output(ctx, mirror, "rev-list", "--count", "refs/heads/main")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return n
}

func localHarness() string {
	if c := harness.FirstAvailable(); c != nil {
		return string(c.Name)
	}
	return ""
}
