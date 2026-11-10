package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/detect"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/gitserver"
	"github.com/nwylynko/clubhouse-cli/netbind"
)

type HostProvider interface {
	ProvisionForStack(ctx context.Context, stack string) (*ProvisionedHost, error)
}

type ProvisionedHost struct {
	ID      string
	WorkDir string
}

type Survivor struct {
	Name    string
	Machine string
	Mirror  string
	Work    string
	Branch  string
	Profile detect.Profile
}

type Request struct {
	PIN          string
	Survivors    []Survivor
	Control      *controlplane.Client
	HostProvider HostProvider
	Pick         func(ranked []Survivor) (Survivor, error)
	DetectGone   func() bool
	SessionNet   *gitserver.SessionNet
}

type Result struct {
	NewHost         Survivor
	Canonical       *canonical.Host
	Provisioned     bool
	ProvisionedHost *ProvisionedHost
}

type Detection struct {
	FailThreshold int
	Window        time.Duration
	fails         []time.Time
}

func (d *Detection) ObserveReachability(ok bool) bool {
	if d.FailThreshold <= 0 {
		d.FailThreshold = 3
	}
	if d.Window <= 0 {
		d.Window = 15 * time.Second
	}
	now := time.Now()
	if ok {
		d.fails = nil
		return false
	}
	d.fails = append(d.fails, now)
	cut := now.Add(-d.Window)
	i := 0
	for i < len(d.fails) && d.fails[i].Before(cut) {
		i++
	}
	d.fails = d.fails[i:]
	return len(d.fails) >= d.FailThreshold
}

func Run(ctx context.Context, req Request) (*Result, error) {
	if len(req.Survivors) == 0 {
		if req.HostProvider == nil {
			return nil, fmt.Errorf("no survivors and no host provider")
		}
		box, err := req.HostProvider.ProvisionForStack(ctx, "static")
		if err != nil {
			return nil, err
		}
		host, err := canonical.Create(ctx, filepath.Join(box.WorkDir, "canonical"), req.PIN)
		if err != nil {
			return nil, err
		}
		host.Bind = bindFor(req.Control)
		host.Session = req.SessionNet
		url, _ := host.StartServer()
		if su := host.SessionRemoteURL(); su != "" {
			url = su
		}
		if url == "" {
			url = host.Bare
		}
		if req.Control != nil {
			if err := flipControl(ctx, req.Control, req.PIN, url, "cloud:"+box.ID); err != nil {
				return nil, err
			}
		}
		return &Result{
			NewHost:         Survivor{Name: "cloud:" + box.ID},
			Canonical:       host,
			Provisioned:     true,
			ProvisionedHost: box,
		}, nil
	}

	ranked := rankSurvivors(req.Survivors)
	pickFn := req.Pick
	if pickFn == nil {
		pickFn = func(r []Survivor) (Survivor, error) { return r[0], nil }
	}
	chosen, err := pickFn(ranked)
	if err != nil {
		return nil, err
	}

	dest := filepath.Join(filepath.Dir(chosen.Mirror), "canonical-new")
	_ = os.RemoveAll(dest)
	host, err := reconstruct(ctx, dest, req.PIN, ranked, chosen)
	if err != nil {
		return nil, err
	}
	host.Bind = bindFor(req.Control)
	host.Session = req.SessionNet
	url, err := host.StartServer()
	if req.SessionNet != nil && err != nil {
		return nil, fmt.Errorf("serve the reconstructed canonical on the session network: %w", err)
	}
	if su := host.SessionRemoteURL(); su != "" {
		url = su
	}
	if url == "" {
		url = "file://" + host.Bare
	}
	if req.Control != nil {
		if err := flipControl(ctx, req.Control, req.PIN, url, chosen.Machine); err != nil {
			return nil, err
		}
	}
	return &Result{NewHost: chosen, Canonical: host}, nil
}

func bindFor(cp *controlplane.Client) string {
	if cp == nil {
		return ""
	}
	return netbind.BindForControl(cp.Base)
}

func flipControl(ctx context.Context, cp *controlplane.Client, pin, newGitURL, hostMachine string) error {
	sess, err := cp.Session(ctx, pin)
	if err != nil {
		return fmt.Errorf("migrate cutover session: %w", err)
	}
	if _, err := cp.Cutover(ctx, pin, controlplane.CutoverRequest{
		NewGitURL:   newGitURL,
		Generation:  sess.Generation,
		HostMachine: hostMachine,
	}); err != nil {
		return fmt.Errorf("migrate cutover: %w", err)
	}
	return nil
}

func rankSurvivors(ss []Survivor) []Survivor {
	profiles := make([]detect.Profile, len(ss))
	for i, s := range ss {
		p := s.Profile
		p.Hostname = s.Name
		profiles[i] = p
	}
	rankedP := detect.Rank(profiles)
	byName := map[string]Survivor{}
	for _, s := range ss {
		byName[s.Name] = s
	}
	out := make([]Survivor, 0, len(ss))
	for _, p := range rankedP {
		if s, ok := byName[p.Hostname]; ok {
			s.Profile = p
			out = append(out, s)
		}
	}
	return out
}

func reconstruct(ctx context.Context, dest, pin string, all []Survivor, chosen Survivor) (*canonical.Host, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	best := chosen.Mirror
	var bestTime int64
	for _, s := range all {
		t := commitTime(ctx, s.Mirror, "main")
		if t >= bestTime {
			bestTime = t
			best = s.Mirror
		}
	}
	bare := filepath.Join(dest, canonical.BareDir)
	if err := sbGit.Run(ctx, "", "clone", "--bare", best, bare); err != nil {
		return nil, fmt.Errorf("clone freshest main: %w", err)
	}
	_ = sbGit.Run(ctx, bare, "config", "http.receivepack", "true")

	for _, s := range all {
		if s.Branch == "" || s.Work == "" {
			continue
		}
		tmp := filepath.Join(dest, ".import-"+s.Name)
		_ = os.RemoveAll(tmp)
		if err := sbGit.Run(ctx, "", "clone", s.Work, tmp); err != nil {
			continue
		}
		c := &sbGit.Cmd{Dir: tmp}
		_ = c.Run(ctx, "remote", "remove", "new")
		_ = c.Run(ctx, "remote", "add", "new", bare)
		_ = c.Run(ctx, "push", "new", "HEAD:"+s.Branch)
		_ = os.RemoveAll(tmp)
	}

	work := filepath.Join(dest, canonical.WorkDir)
	if err := sbGit.Run(ctx, "", "clone", "--branch", "main", bare, work); err != nil {
		return nil, err
	}
	return &canonical.Host{
		Root: dest,
		PIN:  pin,
		Bare: bare,
		Work: work,
	}, nil
}

func commitTime(ctx context.Context, bare, ref string) int64 {
	out, err := sbGit.Output(ctx, bare, "log", "-1", "--format=%ct", ref)
	if err != nil {
		return 0
	}
	var t int64
	fmt.Sscanf(out, "%d", &t)
	return t
}
