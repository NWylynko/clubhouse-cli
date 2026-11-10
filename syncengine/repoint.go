package syncengine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/logx"
)

var repointLog = logx.New("repoint")

const prevRemote = "origin-prev"

type Cursors struct {
	Endpoint   string `json:"endpoint,omitempty"`
	Generation int    `json:"generation,omitempty"`
}

func OriginURL(ctx context.Context, repo string) (string, error) {
	out, err := sbGit.Output(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func Repoint(ctx context.Context, work, mirror, newRemote string, id sbGit.Identity) error {
	_ = id
	if newRemote == "" {
		return fmt.Errorf("repoint: empty remote")
	}
	cur, err := OriginURL(ctx, work)
	if err != nil {
		return fmt.Errorf("repoint: read origin: %w", err)
	}
	if urlsEqual(cur, newRemote) {
		return nil
	}
	if err := sbGit.Run(ctx, "", "ls-remote", newRemote); err != nil {
		return fmt.Errorf("repoint: new endpoint unreachable, leaving origin at %s: %w", cur, err)
	}
	if err := setOrigin(ctx, work, cur, newRemote); err != nil {
		return err
	}
	if mirror != "" {
		mcur, _ := OriginURL(ctx, mirror)
		if err := setOrigin(ctx, mirror, mcur, newRemote); err != nil {
			_ = sbGit.Run(ctx, work, "remote", "set-url", "origin", cur)
			return fmt.Errorf("repoint mirror: %w", err)
		}
	}
	repointLog.Infof("origin %s → %s", cur, newRemote)
	return nil
}

func setOrigin(ctx context.Context, repo, prev, next string) error {
	if prev != "" && !urlsEqual(prev, next) {
		_ = sbGit.Run(ctx, repo, "remote", "remove", prevRemote)
		_ = sbGit.Run(ctx, repo, "remote", "add", prevRemote, prev)
	}
	return sbGit.Run(ctx, repo, "remote", "set-url", "origin", next)
}

type FollowOpts struct {
	Work, Mirror, PIN, Cursors string
	Resolve                    func(ctx context.Context, pin string) (dialAddr string, generation int, err error)
	ID                         sbGit.Identity
}

func FollowHost(ctx context.Context, opt FollowOpts) (repointed bool, err error) {
	if opt.Resolve == nil || opt.PIN == "" {
		return false, nil
	}
	dial, gen, err := opt.Resolve(ctx, opt.PIN)
	if err != nil || dial == "" {
		return false, nil
	}
	cached := LoadCursors(opt.Cursors)
	if gen > 0 && cached.Generation == gen && urlsEqual(cached.Endpoint, dial) {
		return false, nil
	}
	if gen == 0 && urlsEqual(cached.Endpoint, dial) {
		return false, nil
	}
	origin, _ := OriginURL(ctx, opt.Work)
	if !urlsEqual(origin, dial) {
		if err := Repoint(ctx, opt.Work, opt.Mirror, dial, opt.ID); err != nil {
			repointLog.Warnf("follow %s: %v (keeping current origin)", opt.PIN, err)
			return false, nil
		}
		repointed = true
	}
	_ = SaveCursors(opt.Cursors, Cursors{Endpoint: dial, Generation: gen})
	return repointed, nil
}

func CatchUp(ctx context.Context, fromURL, toURL string) error {
	if fromURL == "" || toURL == "" {
		return fmt.Errorf("catch-up: need both from and to URLs")
	}
	if urlsEqual(fromURL, toURL) {
		return nil
	}
	tmp, err := os.MkdirTemp("", "clubhouse-catchup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bare := filepath.Join(tmp, "bare.git")
	if err := sbGit.Run(ctx, "", "clone", "--bare", fromURL, bare); err != nil {
		return fmt.Errorf("catch-up clone: %w", err)
	}
	if err := sbGit.Run(ctx, bare, "push", toURL, "+refs/heads/main:refs/heads/main"); err != nil {
		return fmt.Errorf("catch-up push main: %w", err)
	}
	return nil
}

func LoadCursors(path string) Cursors {
	if path == "" {
		return Cursors{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Cursors{}
	}
	var c Cursors
	_ = json.Unmarshal(b, &c)
	return c
}

func SaveCursors(path string, c Cursors) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func urlsEqual(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}
