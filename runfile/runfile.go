package runfile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/nwylynko/clubhouse-cli/canonical"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
)

const Path = ".club/run.json"

type File struct {
	Install []string
	Dev     []string
}

type wire struct {
	Install string `json:"install,omitempty"`
	Dev     string `json:"dev,omitempty"`
}

func Read(workDir string) File {
	data, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(Path)))
	if err != nil {
		return File{}
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return File{}
	}
	return File{Install: strings.Fields(w.Install), Dev: strings.Fields(w.Dev)}
}

func ReadFromMain(ctx context.Context, host *canonical.Host) File {
	if host == nil {
		return File{}
	}
	out, err := sbGit.Output(ctx, host.Bare, "show", "main:"+Path)
	if err != nil {
		return File{}
	}
	var w wire
	if err := json.Unmarshal([]byte(out), &w); err != nil {
		return File{}
	}
	return File{Install: strings.Fields(w.Install), Dev: strings.Fields(w.Dev)}
}

func Resolve(flag, committed []string, detect func() []string) []string {
	if len(flag) > 0 {
		return flag
	}
	if len(committed) > 0 {
		return committed
	}
	if detect == nil {
		return nil
	}
	return detect()
}

func Commit(ctx context.Context, host *canonical.Host, id sbGit.Identity, install, dev []string) error {
	if host == nil || (len(install) == 0 && len(dev) == 0) {
		return nil
	}
	body, err := json.MarshalIndent(wire{
		Install: strings.Join(install, " "),
		Dev:     strings.Join(dev, " "),
	}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	src := host.Bare
	if host.Remote != "" {
		src = host.Remote
	}
	tmp, err := os.MkdirTemp("", "clubhouse-runfile-*")
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
	if existing, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(Path))); err == nil &&
		strings.TrimSpace(string(existing)) == strings.TrimSpace(string(body)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".club"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(Path)), body, 0o644); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", Path); err != nil {
		return err
	}
	if err := c.Run(ctx, "commit", "-m", "run: how this project installs and starts"); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}
