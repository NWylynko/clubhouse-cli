package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

var Trace func(dir string, args []string, dur time.Duration, err error)

var hermeticConfig = []string{
	"core.autocrlf=false",
	"core.safecrlf=false",
	"core.eol=lf",
	"core.filemode=true",
	"merge.conflictstyle=merge",
	"merge.ff=false",
	"rerere.enabled=false",
	"rerere.autoupdate=false",
	"advice.detachedHead=false",
	"init.defaultBranch=main",
}

func HermeticConfig() []string {
	out := make([]string, len(hermeticConfig))
	copy(out, hermeticConfig)
	return out
}

func Env(extra ...string) []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=" + HermeticIdentity.Name,
		"GIT_AUTHOR_EMAIL=" + HermeticIdentity.Email,
		"GIT_COMMITTER_NAME=" + HermeticIdentity.Name,
		"GIT_COMMITTER_EMAIL=" + HermeticIdentity.Email,
		"LANG=C",
		"LC_ALL=C",
		"PATH=/usr/bin:/bin",
	}
	if h := os.Getenv("HOME"); h != "" {
		env = append(env, "HOME="+h)
	}
	if p := os.Getenv("TMPDIR"); p != "" {
		env = append(env, "TMPDIR="+p)
	}
	return append(env, extra...)
}

type Cmd struct {
	Dir    string
	Env    []string
	Stdin  []byte
	Stdout *bytes.Buffer
	Stderr *bytes.Buffer
}

func (c *Cmd) Run(ctx context.Context, args ...string) error {
	bin, err := Bin()
	if err != nil {
		return err
	}
	full := make([]string, 0, len(hermeticConfig)*2+len(args))
	for _, kv := range hermeticConfig {
		full = append(full, "-c", kv)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = c.Dir
	cmd.Env = Env(c.Env...)
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var stdout, stderr bytes.Buffer
	outW, errW := &stdout, &stderr
	if c.Stdout != nil {
		outW = c.Stdout
	}
	if c.Stderr != nil {
		errW = c.Stderr
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	var start time.Time
	if Trace != nil {
		start = time.Now()
	}
	runErr := cmd.Run()
	if Trace != nil {
		Trace(c.Dir, args, time.Since(start), runErr)
	}
	if runErr != nil {
		msg := strings.TrimSpace(errW.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

func (c *Cmd) Output(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(ctx, args...); err != nil {
		return stdout.String(), err
	}
	return stdout.String(), nil
}

func Run(ctx context.Context, dir string, args ...string) error {
	return (&Cmd{Dir: dir}).Run(ctx, args...)
}

func Output(ctx context.Context, dir string, args ...string) (string, error) {
	return (&Cmd{Dir: dir}).Output(ctx, args...)
}
