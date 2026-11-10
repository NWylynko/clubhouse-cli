package cli

import (
	"fmt"

	"github.com/nwylynko/clubhouse-cli/harness"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/spf13/cobra"
)

func newHarnessVerbCmd(n harness.Name) *cobra.Command {
	verb := string(n)
	c := &cobra.Command{
		Use:   verb + " [pin] [-- args...]",
		Short: "Start " + verb + " in the session work tree (`club open` + " + verb + " in one verb)",
		Long: "Shorthand for `club open` followed by `" + verb + "`: picks the live session\n" +
			"the way `open` does, then runs " + verb + " in its work tree with $CLUB_PIN set\n" +
			"and club on PATH. Pass a PIN to choose a session, and put anything for\n" +
			verb + " itself after `--` so its flags never collide with club's.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHarnessVerb(cmd, n, args)
		},
	}
	return c
}

func runHarnessVerb(cmd *cobra.Command, n harness.Name, args []string) error {
	pinArgs, passthrough := splitAtDash(cmd, args)
	if len(pinArgs) > 1 {
		return fmt.Errorf("%s takes at most a PIN — put %s's own arguments after `--`", n, n)
	}
	bin, ok := harnessBin(n)
	if !ok {
		return fmt.Errorf("%s is not installed on this machine — install its CLI and run `club %s` again (`club open` gives you the session work tree without it)", n, n)
	}
	pin, err := resolveOpenPIN(cmd, pinArgs)
	if err != nil {
		return err
	}
	workPath := workDir(session.ForPin(pin))
	fmt.Fprintf(cmd.ErrOrStderr(), "session %s — running %s in %s\n", pin, n, workPath)
	return spawnInSessionWork(cmd, pin, workPath, bin, passthrough)
}

func splitAtDash(cmd *cobra.Command, args []string) (mine, theirs []string) {
	at := cmd.ArgsLenAtDash()
	if at < 0 {
		return args, nil
	}
	return args[:at], args[at:]
}

func harnessBin(n harness.Name) (string, bool) {
	for _, a := range harness.Available() {
		if a.Name == n {
			return a.Bin, a.Present
		}
	}
	return "", false
}
