package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/spf13/cobra"
)

func newOpenCmd() *cobra.Command {
	open := &cobra.Command{
		Use:     "open [pin]",
		Aliases: []string{"cd", "workspace"},
		Short:   "Enter a session work tree in a subshell (exit to leave)",
		Long: "Opens your $SHELL in the session work tree; exit returns to this\n" +
			"shell. One live session is picked automatically; several show a menu.\n" +
			"Pass a PIN to open that session from local disk. Use --print to emit only\n" +
			"the work path for scripts.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			printPath, _ := cmd.Flags().GetBool("print")
			return runOpen(cmd, args, printPath)
		},
	}
	open.Flags().Bool("print", false, "print the work path and exit (no subshell)")
	return open
}

func runOpen(cmd *cobra.Command, args []string, printPath bool) error {
	out := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()
	pin, err := resolveOpenPIN(cmd, args)
	if err != nil {
		return err
	}
	workPath := workDir(session.ForPin(pin))
	if cwd, _ := os.Getwd(); cwd != "" && alreadyInSessionWork(pin, workPath, cwd) {
		fmt.Fprintf(errW, "already in %s work tree\n", pin)
		if printPath || !openTTY(cmd) {
			fmt.Fprintln(out, workPath)
		}
		return nil
	}
	if !printPath && openTTY(cmd) {
		fmt.Fprintf(errW, "session %s — exit to leave\n", pin)
		return spawnShell(cmd, pin, workPath)
	}
	fmt.Fprintln(out, workPath)
	return nil
}

func resolveOpenPIN(cmd *cobra.Command, args []string) (string, error) {
	errW := cmd.ErrOrStderr()
	if len(args) == 1 {
		pin := args[0]
		if err := awaitWorkTree(cmd, pin); err != nil {
			return "", err
		}
		if _, err := session.Load(pin); err != nil {
			return "", fmt.Errorf("no local session %s — run club to host or club join %s", pin, pin)
		}
		live := livePinSet()
		if !live[pin] {
			fmt.Fprintf(errW, "session %s is not live — mirror may be stale\n", pin)
		}
		return pin, nil
	}
	entries, err := session.LivePins()
	if err != nil {
		return "", err
	}
	switch len(entries) {
	case 0:
		return "", fmt.Errorf("no live session on this machine — run club to host or club join <pin>")
	case 1:
		pin := entries[0].PIN
		if err := awaitWorkTree(cmd, pin); err != nil {
			return "", err
		}
		fmt.Fprintf(errW, "opening %s → %s\n", pin, workDir(session.ForPin(pin)))
		return pin, nil
	default:
		if !openTTY(cmd) {
			return "", fmt.Errorf("%d live sessions — pass club open <pin> or run on a terminal", len(entries))
		}
		pin, err := PickLiveSession(cmd, entries)
		if err != nil {
			return "", err
		}
		if err := awaitWorkTree(cmd, pin); err != nil {
			return "", err
		}
		return pin, nil
	}
}

func awaitWorkTree(cmd *cobra.Command, pin string) error {
	if !session.StandingUp(pin) {
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "session %s is still standing up — waiting for its work tree\n", pin)
	return session.WaitUntilReady(cmd.Context(), pin)
}

func livePinSet() map[string]bool {
	entries, err := session.LivePins()
	if err != nil {
		return nil
	}
	m := make(map[string]bool, len(entries))
	for _, e := range entries {
		m[e.PIN] = true
	}
	return m
}

func PickLiveSession(cmd *cobra.Command, entries []session.LiveEntry) (string, error) {
	errW := cmd.ErrOrStderr()
	for i, e := range entries {
		fmt.Fprintf(errW, "[%d] %s — %s — %s\n", i+1, e.PIN, e.Live.Role, pickerBranch(e))
	}
	fmt.Fprint(errW, "pick > ")
	sc := bufio.NewScanner(cmd.InOrStdin())
	if !sc.Scan() {
		return "", fmt.Errorf("no selection")
	}
	ans := strings.TrimSpace(sc.Text())
	n, err := strconv.Atoi(ans)
	if err != nil || n < 1 || n > len(entries) {
		return "", fmt.Errorf("invalid pick %q", ans)
	}
	pin := entries[n-1].PIN
	fmt.Fprintf(errW, "opening %s → %s\n", pin, workDir(session.ForPin(pin)))
	return pin, nil
}

func pickerBranch(e session.LiveEntry) string {
	if e.Live.Branch != "" {
		return e.Live.Branch
	}
	if s, err := session.Load(e.PIN); err == nil && s.Branch != "" {
		return session.BranchLabel(s.Branch)
	}
	return "-"
}

func alreadyInSessionWork(pin, workPath, dir string) bool {
	if pinFromCwd() != pin {
		return false
	}
	return dirInside(resolvePath(workPath), resolvePath(dir))
}

func dirInside(base, dir string) bool {
	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func openTTY(cmd *cobra.Command) bool {
	in, _ := cmd.InOrStdin().(*os.File)
	out, _ := cmd.OutOrStdout().(*os.File)
	return isCharDevice(in) && isCharDevice(out)
}

func spawnShell(cmd *cobra.Command, pin, workPath string) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return spawnInSessionWork(cmd, pin, workPath, shell, nil)
}

func spawnInSessionWork(cmd *cobra.Command, pin, workPath, bin string, args []string) error {
	env := append(os.Environ(), "CLUB_PIN="+pin)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if filepath.Base(exe) != "club" {
		binDir, err := os.MkdirTemp("", "clubhouse-open-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(binDir)
		link := filepath.Join(binDir, "club")
		if err := os.Symlink(exe, link); err != nil {
			script := "#!/bin/sh\nexec " + strconv.Quote(exe) + " \"$@\"\n"
			if err := os.WriteFile(link, []byte(script), 0o755); err != nil {
				return err
			}
		}
		env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	c := exec.Command(bin, args...)
	c.Dir = workPath
	c.Stdin = cmd.InOrStdin()
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	c.Env = env
	return c.Run()
}
