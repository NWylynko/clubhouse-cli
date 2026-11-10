package box

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type SSHRunner struct {
	Addr    string
	Options []string
}

func (s *SSHRunner) Target() string { return s.Addr }

func (s *SSHRunner) args(extra ...string) []string {
	a := []string{}
	for _, o := range s.Options {
		a = append(a, "-o", o)
	}
	a = append(a, s.Addr)
	return append(a, extra...)
}

func (s *SSHRunner) Run(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", s.args("sh", "-c", shQuote(command))...)
	return combined(cmd, nil)
}

func (s *SSHRunner) Stream(ctx context.Context, command string, in io.Reader, out, errW io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", s.args("sh", "-c", shQuote(command))...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errW
	return cmd.Run()
}

func (s *SSHRunner) Put(ctx context.Context, data []byte, remotePath string) error {
	return s.PutMode(ctx, data, remotePath, 0o644)
}

func (s *SSHRunner) PutMode(ctx context.Context, data []byte, remotePath string, mode os.FileMode) error {
	dir := filepath.Dir(remotePath)
	mkdir := exec.CommandContext(ctx, "ssh", s.args("mkdir", "-p", shQuote(dir))...)
	if out, err := combined(mkdir, nil); err != nil {
		return fmt.Errorf("mkdir %s: %w\n%s", dir, err, out)
	}
	script := fmt.Sprintf("umask 077 && cat > %s && chmod %o %s",
		shSingle(remotePath), mode.Perm(), shSingle(remotePath))
	write := exec.CommandContext(ctx, "ssh", s.args("sh", "-c", shQuote(script))...)
	out, err := combined(write, data)
	if err != nil {
		return fmt.Errorf("write %s: %w\n%s", remotePath, err, out)
	}
	return nil
}

type LocalRunner struct{}

func (LocalRunner) Target() string { return "localhost" }

func (LocalRunner) Run(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	return combined(cmd, nil)
}

func (LocalRunner) Stream(ctx context.Context, command string, in io.Reader, out, errW io.Writer) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errW
	return cmd.Run()
}

func (r LocalRunner) Put(ctx context.Context, data []byte, remotePath string) error {
	return r.PutMode(ctx, data, remotePath, 0o755)
}

func (LocalRunner) PutMode(_ context.Context, data []byte, remotePath string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(remotePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(remotePath, data, mode)
}

func combined(cmd *exec.Cmd, stdin []byte) (string, error) {
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func shQuote(s string) string { return shSingle(s) }

func shSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
