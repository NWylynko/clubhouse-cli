package canonical

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/gitserver"
	"github.com/nwylynko/clubhouse-cli/logx"
)

const (
	BareDir = "bare.git"
	WorkDir = "work"
)

type Host struct {
	Root string
	PIN  string
	Bare string
	Work string
	Srv  *gitserver.Server

	Bind string

	Session *gitserver.SessionNet

	Remote string
}

func SeedReadme(pin string) string { return "# club session " + pin + "\n" }

func Create(ctx context.Context, root, pin string) (*Host, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	h := &Host{
		Root: abs,
		PIN:  pin,
		Bare: filepath.Join(abs, BareDir),
		Work: filepath.Join(abs, WorkDir),
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	id := sbGit.SessionIdentity("host", pin)

	if err := sbGit.Run(ctx, "", "init", "--bare", h.Bare); err != nil {
		return nil, err
	}
	_ = sbGit.Run(ctx, h.Bare, "config", "http.receivepack", "true")
	_ = sbGit.Run(ctx, h.Bare, "config", "receive.denyCurrentBranch", "ignore")

	tmp := filepath.Join(abs, ".seed")
	_ = os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", h.Bare, tmp); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte(SeedReadme(pin)), 0o644); err != nil {
		return nil, err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: id.EnvVars()}
	if err := c.Run(ctx, "add", "README.md"); err != nil {
		return nil, err
	}
	if err := c.Run(ctx, "commit", "-m", "seed canonical"); err != nil {
		return nil, err
	}
	if err := c.Run(ctx, "branch", "-M", "main"); err != nil {
		return nil, err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(tmp)

	if err := sbGit.Run(ctx, "", "clone", "--branch", "main", h.Bare, h.Work); err != nil {
		return nil, err
	}
	h.Srv = &gitserver.Server{Bare: h.Bare}
	return h, nil
}

func Open(root, pin string) (*Host, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	h := &Host{
		Root: abs,
		PIN:  pin,
		Bare: filepath.Join(abs, BareDir),
		Work: filepath.Join(abs, WorkDir),
		Srv:  &gitserver.Server{Bare: filepath.Join(abs, BareDir)},
	}
	if _, err := os.Stat(h.Bare); err != nil {
		return nil, fmt.Errorf("canonical bare missing at %s: %w", h.Bare, err)
	}
	return h, nil
}

func OpenRemote(ctx context.Context, root, pin, remoteURL string) (*Host, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	h := &Host{
		Root:   abs,
		PIN:    pin,
		Bare:   filepath.Join(abs, BareDir),
		Work:   filepath.Join(abs, WorkDir),
		Remote: remoteURL,
	}
	if _, err := os.Stat(h.Bare); err != nil {
		if err := sbGit.Run(ctx, "", "clone", "--mirror", remoteURL, h.Bare); err != nil {
			return nil, fmt.Errorf("mirror canonical from %s: %w", remoteURL, err)
		}
	} else if err := pointReplicaOriginAt(ctx, h.Bare, remoteURL); err != nil {
		return nil, fmt.Errorf("point the canonical mirror at %s: %w", remoteURL, err)
	}
	if _, err := os.Stat(h.Work); err != nil {
		if err := sbGit.Run(ctx, "", "clone", "--branch", "main", remoteURL, h.Work); err != nil {
			return nil, fmt.Errorf("clone canonical work from %s: %w", remoteURL, err)
		}
	} else if err := pointReplicaOriginAt(ctx, h.Work, remoteURL); err != nil {
		return nil, fmt.Errorf("point the canonical work clone at %s: %w", remoteURL, err)
	}
	if err := h.Refresh(ctx); err != nil {
		return nil, err
	}
	return h, nil
}

func pointReplicaOriginAt(ctx context.Context, repo, remoteURL string) error {
	cur, err := sbGit.Output(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("read origin: %w", err)
	}
	if strings.TrimSpace(cur) == remoteURL {
		return nil
	}
	if err := sbGit.Run(ctx, repo, "remote", "set-url", "origin", remoteURL); err != nil {
		return err
	}
	logx.New("canonical").Infof("replica %s follows canonical to %s (was %s)", repo, remoteURL, strings.TrimSpace(cur))
	return nil
}

func (h *Host) Refresh(ctx context.Context) error {
	if h.Remote == "" || h.Bare == "" {
		return nil
	}
	return sbGit.Run(ctx, h.Bare, "remote", "update", "--prune")
}

func (h *Host) StartServer() (string, error) {
	if h.Srv == nil {
		h.Srv = &gitserver.Server{Bare: h.Bare}
	}
	if h.Srv.Bind == "" {
		h.Srv.Bind = h.Bind
	}
	if h.Srv.Session == nil {
		h.Srv.Session = h.Session
	}
	return h.Srv.Start("canonical.git")
}

func (h *Host) SessionRemoteURL() string {
	if h.Srv == nil {
		return ""
	}
	return h.Srv.SessionURL()
}

func (h *Host) RemoteURL() string {
	if h.Remote != "" {
		return h.Remote
	}
	if h.Srv != nil && h.Srv.URL() != "" {
		return h.Srv.URL()
	}
	return h.Bare
}

func (h *Host) Close(ctx context.Context) error {
	if h.Srv == nil {
		return nil
	}
	return h.Srv.Close(ctx)
}

func (h *Host) CreateClientBranch(ctx context.Context, name string) error {
	return sbGit.Run(ctx, h.Bare, "branch", name, "main")
}

func (h *Host) BranchesAheadOfMain(ctx context.Context) ([]string, error) {
	out, err := sbGit.Output(ctx, h.Bare, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var ahead []string
	for _, b := range strings.Split(strings.TrimSpace(out), "\n") {
		b = strings.TrimSpace(b)
		if b == "" || b == "main" {
			continue
		}
		if err := sbGit.Run(ctx, h.Bare, "merge-base", "--is-ancestor", b, "main"); err != nil {
			ahead = append(ahead, b)
		}
	}
	return ahead, nil
}

func (h *Host) MergeBase(ctx context.Context, branch string) (string, error) {
	out, err := sbGit.Output(ctx, h.Bare, "merge-base", "main", branch)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (h *Host) SyncWorkToMain(ctx context.Context) error {
	c := &sbGit.Cmd{Dir: h.Work}
	if err := c.Run(ctx, "fetch", "origin", "main"); err != nil {
		return err
	}
	if logx.DebugEnabled() {
		if status, err := c.Output(ctx, "status", "--porcelain"); err == nil && strings.TrimSpace(status) != "" {
			logx.New("canonical").Debugf("demo mirror %s had local modifications, overwriting with main:\n%s", h.Work, strings.TrimSpace(status))
		}
	}
	return c.Run(ctx, "reset", "--hard", "origin/main")
}
