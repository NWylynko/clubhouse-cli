package conductor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/contracts"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/runtime"
)

var setupLog = logx.New("setup")

const MarkerFile = ".club/setup.done"

const setupTimeout = 10 * time.Minute

type Scaffolder func(ctx context.Context, dir, prompt string, onActivity func(string)) error

type Setup struct {
	Host    *canonical.Host
	ID      sbGit.Identity
	Agent   Scaffolder
	Harness string
	Control *controlplane.Client
	PIN     string
	States  *StateBoard

	mu           sync.Mutex
	saidDisabled bool
}

func (s *Setup) Name() string { return "setup" }

func (s *Setup) harnessName() string {
	if s.Harness == "" {
		return "harness"
	}
	return s.Harness
}

func (s *Setup) Tick(ctx context.Context) error {
	if s.Host == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	brief := strings.TrimSpace(s.showMain(ctx, contracts.BriefFile))
	if brief == "" {
		return nil
	}
	if s.marked(ctx) {
		return nil
	}
	if s.Agent == nil {
		if !s.saidDisabled {
			s.saidDisabled = true
			setupLog.Infof("setup disabled: no harness CLI — the brief stays on main, so every joined agent's contract still carries it")
		}
		return nil
	}

	files := s.projectFiles(ctx)
	mode, prompt := "scaffold", scaffoldPrompt(brief)
	if len(files) > 0 {
		mode, prompt = "adapt", adaptPrompt(brief)
	}
	setupLog.Infof("%s mode — %s working from the brief: %s", mode, s.harnessName(), oneLine(brief))
	s.States.Working(RoleSetup, fmt.Sprintf("%s (%s): %s", s.harnessName(), mode, oneLine(brief)))
	defer s.States.Idle(RoleSetup)
	onActivity := func(a string) {
		if a = strings.TrimSpace(a); a != "" {
			s.States.Working(RoleSetup, fmt.Sprintf("%s: %s", s.harnessName(), oneLine(a)))
		}
	}

	src := s.Host.Bare
	if s.Host.Remote != "" {
		src = s.Host.Remote
	}
	tmp, err := os.MkdirTemp("", "clubhouse-setup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", src, tmp); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: s.ID.EnvVars()}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}

	if err := ensureIgnore(tmp); err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	run := func() error { return s.Agent(runCtx, tmp, prompt, onActivity) }
	if mode == "scaffold" {
		run = func() error { return s.scaffoldHandover(runCtx, tmp, prompt, onActivity) }
	}
	if err := run(); err != nil {
		setupLog.Warnf("%s could not %s the project: %v — main untouched, retrying next tick", s.harnessName(), mode, err)
		return err
	}

	if err := ensureIgnore(tmp); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	if err := guardStaged(ctx, tmp); err != nil {
		setupLog.Errorf("refusing to commit the %s: %v", mode, err)
		return err
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".club"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(runtime.SharedEnvPath)), []byte("PORT=3000\n"), 0o644); err != nil {
		return err
	}
	marker := fmt.Sprintf("%s by club's setup role from .club/brief.md\n", mode)
	if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(MarkerFile)), []byte(marker), 0o644); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", runtime.SharedEnvPath, MarkerFile); err != nil {
		return err
	}
	if err := c.Run(ctx, "commit", "-m", "setup: "+mode+" the project from the brief"); err != nil {
		return err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return err
	}
	setupLog.Infof("%s landed on main — every client's next sync pulls the project", mode)
	return nil
}

func (s *Setup) showMain(ctx context.Context, path string) string {
	out, err := sbGit.Output(ctx, s.Host.Bare, "show", "main:"+path)
	if err != nil {
		return ""
	}
	return out
}

func (s *Setup) marked(ctx context.Context) bool {
	_, err := sbGit.Output(ctx, s.Host.Bare, "show", "main:"+MarkerFile)
	return err == nil
}

func (s *Setup) projectFiles(ctx context.Context) []string {
	out, err := sbGit.Output(ctx, s.Host.Bare, "ls-tree", "-r", "--name-only", "main")
	if err != nil {
		return nil
	}
	var paths []string
	for _, p := range strings.Split(out, "\n") {
		p = strings.TrimSpace(p)
		if p == "" || isClubArtifact(p) {
			continue
		}
		if p == "README.md" && strings.TrimSpace(s.showMain(ctx, p)) == strings.TrimSpace(canonical.SeedReadme(s.Host.PIN)) {
			continue
		}
		paths = append(paths, p)
	}
	return paths
}

func isClubArtifact(p string) bool {
	return strings.HasPrefix(p, ".club/") ||
		strings.HasPrefix(p, ".cursor/") ||
		p == "AGENTS.md" || p == "CLAUDE.md" || p == ".gitignore"
}

func WriteBrief(ctx context.Context, host *canonical.Host, id sbGit.Identity, brief string) error {
	brief = strings.TrimSpace(brief)
	if host == nil || brief == "" {
		return nil
	}
	if _, err := sbGit.Output(ctx, host.Bare, "show", "main:"+contracts.BriefFile); err == nil {
		return nil
	}
	src := host.Bare
	if host.Remote != "" {
		src = host.Remote
	}
	tmp, err := os.MkdirTemp("", "clubhouse-brief-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", src, tmp); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: id.EnvVars()}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".club"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(contracts.BriefFile)), []byte(brief+"\n"), 0o644); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", contracts.BriefFile); err != nil {
		return err
	}
	if err := c.Run(ctx, "commit", "-m", "brief: what we're building"); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}

func WriteContracts(ctx context.Context, host *canonical.Host, id sbGit.Identity) error {
	if host == nil {
		return nil
	}
	src := host.Bare
	if host.Remote != "" {
		src = host.Remote
	}
	tmp, err := os.MkdirTemp("", "clubhouse-contracts-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", src, tmp); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: id.EnvVars()}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}
	if err := contracts.Install(tmp, host.PIN); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	staged, err := sbGit.Output(ctx, tmp, "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	if strings.TrimSpace(staged) == "" {
		return nil
	}
	if err := c.Run(ctx, "commit", "-m", "contracts: how agents work in this session"); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}

var ignoreRules = []string{
	"node_modules/", ".next/", "dist/", "build/", "target/",
	"__pycache__/", ".venv/", ".env", ".DS_Store",
}

func IgnoreRules() []string { return append([]string(nil), ignoreRules...) }

func ensureIgnore(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	existing, _ := os.ReadFile(path)
	have := map[string]bool{}
	for _, l := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add []string
	for _, rule := range ignoreRules {
		if !have[rule] && !have[strings.TrimSuffix(rule, "/")] {
			add = append(add, rule)
		}
	}
	if len(add) == 0 {
		return nil
	}
	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if body != "" {
		body += "\n"
	}
	body += "# club: never sync dependencies or build output\n" + strings.Join(add, "\n") + "\n"
	return os.WriteFile(path, []byte(body), 0o644)
}

const (
	maxStagedFiles = 4000
	maxStagedBytes = 40 << 20
)

func guardStaged(ctx context.Context, dir string) error {
	out, err := sbGit.Output(ctx, dir, "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	var files int
	var bytes int64
	for _, p := range strings.Split(out, "\n") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		files++
		if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err == nil {
			bytes += st.Size()
		}
	}
	if files > maxStagedFiles || bytes > maxStagedBytes {
		return fmt.Errorf("staged tree is %d files / %d MiB — that looks like committed dependencies, not a project (limits: %d files / %d MiB)",
			files, bytes>>20, maxStagedFiles, maxStagedBytes>>20)
	}
	return nil
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 120 {
		return s[:117] + "…"
	}
	return s
}

const sharedSetupRules = `
Hard rules:
- Work IN THE CURRENT DIRECTORY. It is an existing git work tree (it has dotfiles). Do not create a subdirectory for the project, do not run "git init", and NEVER run any git command — club makes the commit.
- Do not create, edit or delete anything under .club/.
- Leave the project runnable: a dev script in package.json (or the ecosystem's equivalent) and a committed lockfile.
- Do not start a dev server or any other long-running process. Install dependencies if you need to, but exit when the files are written.
- Dependencies and build output are already gitignored; leave those rules alone.
- The dev server must listen on port 3000 and accept connections from outside localhost — club's box and session-network splice dial 127.0.0.1:3000, not the framework default. Next.js and similar stacks honor PORT=3000; Vite does NOT — set server.port to 3000 and server.host to true in vite.config (or the ecosystem equivalent).
- Teammates open the dev server through club, so it will see requests with a Host header like "<pin>.club.localhost:<port>" rather than localhost. If the framework gates on that (Next.js allowedDevOrigins, Vite server.allowedHosts), allow "*.club.localhost" in the dev config.`

func scaffoldPrompt(brief string) string {
	return fmt.Sprintf(`You are club's setup role-agent. Several AI agents are about to build one product together, and the repository is empty. Create the project they will build in, from this brief:

%s

Prefer the ecosystem's own generator over hand-writing files (for example: npx create-next-app@latest ., npm create vite@latest ., cargo new --vcs none .). Pick sensible defaults rather than asking questions — nothing can answer an interactive prompt.

The directory has been cleared for you: club's own files are held aside so generators that refuse a non-empty directory will run. They come back after this turn, so do not recreate them — if you want to write an AGENTS.md of your own, do, and you will be asked to merge the two.
%s`, brief, sharedSetupRules)
}

func adaptPrompt(brief string) string {
	return fmt.Sprintf(`You are club's setup role-agent. This is someone's EXISTING PROJECT, which they brought into a club session. Move it toward this brief:

%s

Guardrails, because this is not your repository:
- Read the project first and follow its conventions.
- Make the SMALLEST COHERENT SET OF CHANGES that moves it toward the brief.
- NEVER delete or rewrite files the brief does not reach. Prefer additive work — new routes, components, config — over restructuring.
- If the brief is already satisfied, change nothing.
%s`, brief, sharedSetupRules)
}
