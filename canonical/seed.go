package canonical

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	sbGit "github.com/nwylynko/clubhouse-cli/git"
)

var dependencyDirs = []string{
	"node_modules", ".next", "dist", "build", "target", "__pycache__", ".venv",
}

func SeedGuardDirs() []string { return append([]string(nil), dependencyDirs...) }

func PreflightSeedDir(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("--seed %s: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("--seed %s: not a directory", dir)
	}
	if _, err := os.ReadDir(dir); err != nil {
		return fmt.Errorf("--seed %s: %w", dir, err)
	}
	ignored := seedIgnoreRules(dir)
	for _, name := range dependencyDirs {
		if ignored[name] {
			continue
		}
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.IsDir() {
			return fmt.Errorf("--seed %s carries %s/ and nothing ignores it — seeding it would push "+
				"dependencies into canonical and into every client's mirror.\n"+
				"       add %s/ to %s/.gitignore and try again",
				dir, name, name, dir)
		}
	}
	return nil
}

func seedIgnoreRules(dir string) map[string]bool {
	body, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	rules := map[string]bool{}
	for _, l := range strings.Split(string(body), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		rules[strings.Trim(l, "/")] = true
	}
	return rules
}

func SeedRemote(ctx context.Context, gitURL, dir string, id sbGit.Identity) error {
	if gitURL == "" || dir == "" {
		return nil
	}
	if err := PreflightSeedDir(dir); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "clubhouse-seed-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", "--branch", "main", gitURL, tmp); err != nil {
		return fmt.Errorf("clone canonical to seed it: %w", err)
	}
	if err := CopyTree(dir, tmp); err != nil {
		return fmt.Errorf("copy %s into canonical: %w", dir, err)
	}
	c := &sbGit.Cmd{Dir: tmp, Env: id.EnvVars()}
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	status, _ := c.Output(ctx, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		return nil
	}
	if err := c.Run(ctx, "commit", "-m", "seed from "+filepath.Base(dir)); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}

func CopyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
