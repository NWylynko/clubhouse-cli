package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type Name string

const (
	Claude Name = "claude"
	Codex  Name = "codex"
	Cursor Name = "cursor"
)

type Client struct {
	Name  Name
	Model string
	Bin   string

	Dir string

	CompleteFn func(ctx context.Context, system, user string) (string, error)

	Run func(ctx context.Context, bin string, args []string, dir, stdin string) (stdout string, err error)

	AgentFn func(ctx context.Context, dir, prompt string, out io.Writer) error

	OnEvent func(Event)

	RunAgent func(ctx context.Context, bin string, args []string, dir string, out io.Writer) error
}

func Fake(fn func(ctx context.Context, system, user string) (string, error)) *Client {
	return &Client{Name: "fake", Bin: "fake", CompleteFn: fn}
}

func Lookup(name, model string) *Client {
	n := Name(strings.ToLower(strings.TrimSpace(name)))
	if n == "" {
		return nil
	}
	bin := defaultBin(n)
	if bin == "" {
		return nil
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil
	}
	return &Client{Name: n, Model: model, Bin: path}
}

func FirstAvailable() *Client {
	for _, a := range Available() {
		if a.Present {
			return Lookup(string(a.Name), "")
		}
	}
	return nil
}

type Availability struct {
	Name    Name
	Bin     string
	Present bool
}

var All = []Name{Claude, Codex, Cursor}

func Available() []Availability {
	out := make([]Availability, 0, len(All))
	for _, n := range All {
		a := Availability{Name: n}
		if bin := defaultBin(n); bin != "" {
			if path, err := exec.LookPath(bin); err == nil {
				a.Bin, a.Present = path, true
			}
		}
		out = append(out, a)
	}
	return out
}

func IsAvailable(name string) bool {
	want := Name(strings.ToLower(strings.TrimSpace(name)))
	for _, a := range Available() {
		if a.Name == want {
			return a.Present
		}
	}
	return false
}

func SuggestedModels(n Name) []string {
	switch Name(strings.ToLower(string(n))) {
	case Claude:
		return []string{"opus", "sonnet", "haiku"}
	case Codex:
		return []string{"gpt-5-codex", "gpt-5"}
	case Cursor:
		return []string{"auto", "sonnet-4.5", "gpt-5"}
	default:
		return nil
	}
}

func defaultBin(n Name) string {
	switch n {
	case Claude:
		return "claude"
	case Codex:
		return "codex"
	case Cursor:
		if _, err := exec.LookPath("agent"); err == nil {
			return "agent"
		}
		return "cursor-agent"
	default:
		return ""
	}
}

func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("harness: nil client")
	}
	if c.CompleteFn != nil {
		return c.CompleteFn(ctx, system, user)
	}
	args, stdin, err := c.buildArgs(system, user)
	if err != nil {
		return "", err
	}
	run := c.Run
	if run == nil {
		run = defaultRun
	}
	out, err := run(ctx, c.Bin, args, c.Dir, stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (c *Client) buildArgs(system, user string) (args []string, stdin string, err error) {
	switch c.Name {
	case Claude:
		args = []string{
			"-p",
			"--output-format", "text",
			"--system-prompt", system,
			"--dangerously-skip-permissions",
		}
		if c.Model != "" {
			args = append(args, "--model", c.Model)
		}
		args = append(args, user)
		return args, "", nil
	case Cursor:
		args = []string{"-p", "--mode", "ask", "--force", "--output-format", "text"}
		if c.Model != "" {
			args = append(args, "--model", c.Model)
		}
		args = append(args, user)
		if system != "" {
			args[len(args)-1] = system + "\n\n" + user
		}
		return args, "", nil
	case Codex:
		args = []string{"exec", "--skip-git-repo-check"}
		if c.Model != "" {
			args = append(args, "--model", c.Model)
		}
		args = append(args, "-")
		stdin = system + "\n\n" + user
		return args, stdin, nil
	case "fake":
		return nil, "", fmt.Errorf("harness fake client requires CompleteFn")
	default:
		return nil, "", fmt.Errorf("harness: unknown name %q", c.Name)
	}
}

func (c *Client) Agent(ctx context.Context, dir, prompt string, out io.Writer) error {
	if c == nil {
		return fmt.Errorf("harness: nil client")
	}
	if c.AgentFn != nil {
		return c.AgentFn(ctx, dir, prompt, out)
	}
	args, err := c.agentArgs(prompt)
	if err != nil {
		return err
	}
	run := c.RunAgent
	if run == nil {
		run = defaultRunAgent
	}
	if out == nil {
		out = io.Discard
	}
	dec := newStreamDecoder(c.Name, func(e Event) {
		if c.OnEvent != nil {
			c.OnEvent(e)
		}
		_, _ = io.WriteString(out, e.Line()+"\n")
	})
	err = run(ctx, c.Bin, args, dir, dec)
	dec.Close()
	return err
}

func (c *Client) agentArgs(prompt string) ([]string, error) {
	var args []string
	switch c.Name {
	case Claude:
		args = []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	case Codex:
		args = []string{"exec", "--sandbox", "workspace-write", "-c", `approval_policy="never"`, "--skip-git-repo-check"}
	case Cursor:
		args = []string{"-p", "--output-format", "stream-json", "--force"}
	case "fake":
		return nil, fmt.Errorf("harness fake client requires AgentFn")
	default:
		return nil, fmt.Errorf("harness: unknown name %q", c.Name)
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	return append(args, prompt), nil
}

func defaultRunAgent(ctx context.Context, bin string, args []string, dir string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("harness %s: %w", bin, err)
	}
	return nil
}

func defaultRun(ctx context.Context, bin string, args []string, dir, stdin string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("harness %s: %s", bin, msg)
	}
	return stdout.String(), nil
}
