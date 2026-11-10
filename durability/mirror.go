package durability

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sbGit "github.com/nwylynko/clubhouse-cli/git"
)

type Config struct {
	RemoteURL string
	Token     string
}

type Mirror struct {
	Bare   string
	Config Config

	mu       sync.Mutex
	lastPush time.Time
	pushes   int
	lastErr  error
}

func (m *Mirror) Enabled() bool { return m != nil && m.Config.RemoteURL != "" }

func (m *Mirror) Trigger(ctx context.Context) {
	if !m.Enabled() {
		return
	}
	go func() {
		_ = m.PushNow(context.Background())
	}()
}

func (m *Mirror) PushNow(ctx context.Context) error {
	if !m.Enabled() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	url := m.Config.RemoteURL
	if httpRemote(url) && strings.TrimSpace(m.Config.Token) == "" {
		err := fmt.Errorf("no mirror credential: set GITHUB_TOKEN or GH_TOKEN")
		m.lastErr = err
		return err
	}

	remote := "clubhouse-durability"
	_ = sbGit.Run(ctx, m.Bare, "remote", "remove", remote)
	if err := sbGit.Run(ctx, m.Bare, "remote", "add", remote, url); err != nil {
		m.lastErr = err
		return err
	}

	env := []string{"GIT_TERMINAL_PROMPT=0"}
	args := []string{"push", "--force", remote, "main:main"}
	if tok := strings.TrimSpace(m.Config.Token); tok != "" {
		env = append(env, mirrorTokenEnv+"="+tok)
		args = append([]string{
			"-c", "credential.helper=",
			"-c", "credential.helper=" + mirrorCredentialHelper,
		}, args...)
	}
	c := &sbGit.Cmd{Dir: m.Bare, Env: env}
	if err := c.Run(ctx, args...); err != nil {
		m.lastErr = err
		return err
	}
	m.lastPush = time.Now()
	m.pushes++
	m.lastErr = nil
	return nil
}

const mirrorTokenEnv = "CLUB_MIRROR_TOKEN"

const mirrorCredentialHelper = `!f() { echo username=x-access-token; echo password=$` + mirrorTokenEnv + `; }; f`

func httpRemote(url string) bool {
	return strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")
}

func (m *Mirror) Stats() (pushes int, last time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pushes, m.lastPush, m.lastErr
}

func SeedFrom(ctx context.Context, remoteURL, destBare string) error {
	if err := os.MkdirAll(filepath.Dir(destBare), 0o755); err != nil {
		return err
	}
	if err := sbGit.Run(ctx, "", "clone", "--bare", remoteURL, destBare); err != nil {
		return fmt.Errorf("seed from mirror: %w", err)
	}
	return nil
}

func LoadConfig(remoteURL string) Config {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		tok = os.Getenv("GH_TOKEN")
	}
	return Config{RemoteURL: strings.TrimSpace(remoteURL), Token: tok}
}
