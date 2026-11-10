package joindaemon

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/conductor"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/detect"
	"github.com/nwylynko/clubhouse-cli/devserver"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/gitserver"
	"github.com/nwylynko/clubhouse-cli/harness"
	"github.com/nwylynko/clubhouse-cli/hoststart"
	"github.com/nwylynko/clubhouse-cli/migrate"
	"github.com/nwylynko/clubhouse-cli/netbind"
	"github.com/nwylynko/clubhouse-cli/placement"
	"github.com/nwylynko/clubhouse-cli/runfile"
	"github.com/nwylynko/clubhouse-cli/runtime"
	"github.com/nwylynko/clubhouse-cli/sessionnet"
)

func (j *Joined) newPlacementLoop() *placement.Loop {
	return &placement.Loop{
		Control:  j.Control,
		PIN:      j.Session.PIN,
		MemberID: j.MemberID,
		Name:     strings.TrimPrefix(j.Session.Branch, "client/"),
		Machine:  hostnameOr("this machine"),
		Start:    j.startService,
		Stop:     j.stopService,
		Serves: func(service string) bool {
			return !(service == controlplane.ServiceConductor && j.conductsElsewhereInThisProcess)
		},
		OnChange: func(service, detail string) {
			j.log.Infof("placement: %s %s", service, detail)
		},
	}
}

func (j *Joined) startService(ctx context.Context, service string) error {
	switch service {
	case controlplane.ServiceGit:
		return j.promoteToCanonical(ctx)
	case controlplane.ServiceDev:
		return j.startDev(ctx)
	case controlplane.ServiceConductor:
		return j.startConductor(ctx)
	}
	return nil
}

func (j *Joined) stopService(_ context.Context, service string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch service {
	case controlplane.ServiceDev:
		if j.devHolder != nil {
			_ = j.devHolder.Close()
			j.devHolder = nil
		}
		j.devRuntime = nil
		if j.dev != nil {
			_ = j.dev.Stop()
			j.dev = nil
		}
	case controlplane.ServiceConductor:
		if j.fleetLogs != nil {
			j.fleetLogs.Close()
			j.fleetLogs = nil
		}
		j.fleet, j.publisher, j.refreshGate = nil, nil, nil
	case controlplane.ServiceGit:
		if j.canonical != nil && j.canonical.Srv != nil {
			j.canonical.Srv.UnpublishSession()
		}
		j.log.Infof("no longer serving canonical — clients follow the control plane's git endpoint")
	}
	return nil
}

func (j *Joined) promoteToCanonical(ctx context.Context) error {
	j.mu.Lock()
	already := j.canonical
	j.mu.Unlock()
	if already != nil {
		return nil
	}
	profile := detect.Probe()
	sessNet, err := j.sessionNetFor(ctx, "git")
	if err != nil {
		return fmt.Errorf("promote mirror to canonical: %w", err)
	}
	res, err := migrate.Run(ctx, migrate.Request{
		PIN: j.Session.PIN,
		Survivors: []migrate.Survivor{{
			Name:    strings.TrimPrefix(j.Session.Branch, "client/"),
			Machine: hostnameOr("this machine"),
			Mirror:  j.Paths.Mirror,
			Work:    j.Paths.Work,
			Branch:  j.Session.Branch,
			Profile: profile,
		}},
		Control:    j.Control,
		SessionNet: sessNet,
	})
	if err != nil {
		return fmt.Errorf("promote mirror to canonical: %w", err)
	}
	j.mu.Lock()
	j.canonical = res.Canonical
	j.mu.Unlock()
	if su := res.Canonical.SessionRemoteURL(); su != "" {
		j.log.Infof("serving canonical for %s from this machine — %s (session network; local listener %s)", j.Session.PIN, su, res.Canonical.RemoteURL())
	} else {
		j.log.Infof("serving canonical for %s from this machine — %s", j.Session.PIN, res.Canonical.RemoteURL())
	}
	return nil
}

func (j *Joined) sessionNetFor(ctx context.Context, service string) (*gitserver.SessionNet, error) {
	sess, err := j.Control.Session(ctx, j.Session.PIN)
	if err != nil {
		return nil, err
	}
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
	bind := netbind.BindForControl(j.Control.Base)
	return &gitserver.SessionNet{
		Relay: sess.RelayAddr, PIN: sess.PIN, Key: key, Context: context.WithoutCancel(ctx),
		Direct: hoststart.DirectIsPublishable(bind),
		Ticket: j.Control.HolderTicket(sess.PIN, service),
	}, nil
}

func (j *Joined) startDev(ctx context.Context) error {
	work := j.Paths.Work
	j.mu.Lock()
	if j.canonical != nil {
		work = j.canonical.Work
	}
	j.mu.Unlock()

	declared := runfile.Read(work)
	install := runfile.Resolve(nil, declared.Install, func() []string { return devserver.DetectInstall(work) })
	dev := runfile.Resolve(nil, declared.Dev, func() []string { return devserver.DetectDev(work) })
	if len(dev) == 0 {
		return fmt.Errorf("this project declares no dev command (%s) and none could be detected", runfile.Path)
	}
	logs := &devserver.LogBuffer{}
	if len(install) > 0 {
		if err := devserver.Install(ctx, work, install,
			logs.Writer(devserver.StreamStderr, devserver.PhaseInstall)); err != nil {
			j.log.Warnf("install failed (starting dev anyway): %v", err)
		}
	}
	sup := &devserver.Supervisor{WorkDir: work, Command: dev, Logs: logs}
	if err := sup.Start(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	sess, err := j.Control.Session(ctx, j.Session.PIN)
	if err != nil {
		_ = sup.Stop()
		return fmt.Errorf("start dev: read session: %w", err)
	}
	rt := &runtime.Reconciler{
		WorkDir: work, Dev: sup, Control: j.Control, PIN: j.Session.PIN, Generation: sess.Generation,
	}
	var holder *devserver.Holder
	if sessNet, err := j.sessionNetFor(ctx, "dev"); err != nil {
		_ = sup.Stop()
		return fmt.Errorf("start dev: %w", err)
	} else if sessNet != nil {
		port, why := runtime.LocalDevPort(work)
		if port <= 0 {
			_ = sup.Stop()
			return fmt.Errorf("start dev: no local dev port to publish — %s", why)
		}
		opt := devserver.HolderOptions{
			Relay: sessNet.Relay, PIN: sessNet.PIN, Key: sessNet.Key, LocalPort: port, Ticket: sessNet.Ticket,
		}
		if sessNet.Direct {
			if dln, advHost, err := netbind.ListenAdvertise(netbind.BindForControl(j.Control.Base), 0); err == nil {
				opt.DirectListener = dln
				opt.DirectAdvertise = net.JoinHostPort(advHost, strconv.Itoa(dln.Addr().(*net.TCPAddr).Port))
			}
		}
		h, err := devserver.StartHolder(context.WithoutCancel(ctx), opt)
		if err != nil {
			_ = sup.Stop()
			return fmt.Errorf("start dev: %w", err)
		}
		holder = h
		rt.SetSessionDev(h.URL(), h.Direct())
	}
	j.mu.Lock()
	j.dev, j.devHolder, j.devRuntime = sup, holder, rt
	j.mu.Unlock()
	rt.AnnounceDev(ctx)
	j.log.Infof("dev server running here: %s", strings.Join(dev, " "))
	return nil
}

func (j *Joined) keepDevPublished(ctx context.Context) {
	j.mu.Lock()
	sup, rt := j.dev, j.devRuntime
	j.mu.Unlock()
	if sup == nil || rt == nil {
		return
	}
	if sess, err := j.Control.Session(ctx, j.Session.PIN); err == nil && sess.Generation > 0 {
		rt.SetGeneration(sess.Generation)
	}
	if sup.Running() {
		rt.AnnounceDev(ctx)
		return
	}
	if !sup.NeedsRestart() {
		return
	}
	if err := sup.Restart(context.WithoutCancel(ctx)); err != nil {
		j.log.Warnf("dev server %q could not be restarted: %v", strings.Join(sup.Command, " "), err)
		return
	}
	j.log.Infof("dev server %q had exited — restarted it", strings.Join(sup.Command, " "))
	rt.AnnounceDev(ctx)
}

func (j *Joined) startConductor(ctx context.Context) error {
	j.mu.Lock()
	host := j.canonical
	j.mu.Unlock()

	if host == nil {
		published := j.publishedGitEndpoint(ctx)
		url, _, err := j.Control.GitURL(ctx, j.Session.PIN)
		if err != nil {
			return canonical.ExplainRemoteOpenFailure(j.Session.PIN, published, err)
		}
		host, err = canonical.OpenRemote(ctx, filepath.Join(j.Paths.Root, "replica"), j.Session.PIN, url)
		if err != nil {
			return canonical.ExplainRemoteOpenFailure(j.Session.PIN, published, err)
		}
	}
	built := conductor.BuildSessionFleet(ctx, conductor.SessionFleetSpec{
		Host: host, ID: sbGit.SessionIdentity("conductor", j.Session.PIN),
		Control: j.Control, PIN: j.Session.PIN, Fallback: harness.FirstAvailable(),
	})
	if !built.HasIntelligence() {
		return fmt.Errorf("no harness CLI on this machine for any role the session named")
	}
	j.mu.Lock()
	j.fleet, j.fleetHost, j.publisher, j.fleetLogs = built.Fleet, host, built.Publisher, built.Logs
	j.mu.Unlock()
	j.log.Infof("conducting %s from this machine — %s", j.Session.PIN, built.Summary())
	return nil
}

func (j *Joined) publishedGitEndpoint(ctx context.Context) string {
	sess, err := j.Control.Session(ctx, j.Session.PIN)
	if err != nil {
		return ""
	}
	// raw endpoint ok: named for a human, never dialled.
	return sess.Endpoints[controlplane.EndpointGit].URL
}

func (j *Joined) fleetLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-j.stop
		cancel()
	}()
	go j.publishRoleState(ctx)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-j.stop:
			return
		case <-t.C:
			j.tickFleet(ctx)
		}
	}
}

func (j *Joined) publishRoleState(ctx context.Context) {
	t := time.NewTicker(conductor.StatePublishInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.mu.Lock()
			p := j.publisher
			j.mu.Unlock()
			p.Publish(ctx)
		}
	}
}

func (j *Joined) tickFleet(ctx context.Context) {
	j.mu.Lock()
	fleet, host := j.fleet, j.fleetHost
	j.mu.Unlock()
	if fleet == nil {
		return
	}
	if host != nil {
		j.mu.Lock()
		if j.refreshGate == nil {
			j.refreshGate = conductor.NewRefreshGate(j.Control, j.Session.PIN, host.RemoteURL() != "")
		}
		gate := j.refreshGate
		j.mu.Unlock()
		if gate.Due(time.Now()) {
			_ = host.Refresh(ctx)
		}
	}
	if err := fleet.TickRoles(ctx); err != nil {
		j.log.Warnf("conductor tick: %v", err)
	}
	if err := fleet.TickAfter(ctx); err != nil {
		j.log.Warnf("conductor tick: %v", err)
	}
}
