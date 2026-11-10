package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nwylynko/clubhouse-cli/devserver"
	"github.com/nwylynko/clubhouse-cli/runfile"
	"github.com/nwylynko/clubhouse-cli/runtime"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/spf13/cobra"
)

func newSiteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site",
		Short: "Print the URL this machine opens the session's dev server on",
		Long: "Prints the session's site URL and nothing else, so it can be piped.\n\n" +
			"That site serves main — everyone's merged work, from whichever member\n" +
			"holds the dev lease. It is NOT your branch: run `club dev-setup` for\n" +
			"that.",
		Args: cobra.NoArgs,
		RunE: runSiteURL,
	}
	addPinFlag(cmd)
	return cmd
}

func runSiteURL(cmd *cobra.Command, _ []string) error {
	pin := resolvePin(cmd)
	if pin == "" {
		return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set CLUB_PIN")
	}
	marker, live := session.LiveHere(pin)
	if !live {
		return fmt.Errorf("no live club for session %s on this machine — the site URL is held by that process, so start it with `club join %s`", pin, pin)
	}
	if marker.DevURL == "" {
		return fmt.Errorf("session %s has no dev server yet — nothing is serving the project. `club monitor` shows what the session is doing", pin)
	}
	fmt.Fprintln(cmd.OutOrStdout(), marker.DevURL)
	return nil
}

func newDevSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev-setup",
		Short: "Print how to run this branch's dev server on this machine",
		Long: "Running the project here shows YOUR work, including changes you have not\n" +
			"synced yet. The session's own site (`club site`) serves main, so it\n" +
			"cannot show you anything you have not published.",
		Args: cobra.NoArgs,
		RunE: runDevSetup,
	}
	addPinFlag(cmd)
	return cmd
}

func runDevSetup(cmd *cobra.Command, _ []string) error {
	_, paths, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	work := workDir(paths)
	declared := runfile.Read(work)
	install, installFrom := devSetupCommand(declared.Install, devserver.DetectInstall(work))
	dev, devFrom := devSetupCommand(declared.Dev, devserver.DetectDev(work))

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Running this project here shows YOUR branch, including work you have not\n"+
		"synced. The session's site (`club site`) serves main — everyone's merged\n"+
		"work — so it never shows an unsynced change.\n\n")
	fmt.Fprintf(out, "  work tree  %s\n", work)
	fmt.Fprintf(out, "  install    %s%s\n", devSetupValue(install), devFromLabel(installFrom))
	fmt.Fprintf(out, "  dev        %s%s\n", devSetupValue(dev), devFromLabel(devFrom))
	fmt.Fprintf(out, "\nRun them in the work tree. %s\n", devSetupPortAdvice())
	if installFrom == "" && devFrom == "" {
		fmt.Fprintf(out, "\nNothing is declared in %s and nothing recognizable is checked out yet —\n"+
			"use whatever the project's own README or package manifest says.\n", filepath.ToSlash(runfile.Path))
	}
	return nil
}

func devSetupCommand(declared, detected []string) (cmd []string, from string) {
	if len(declared) > 0 {
		return declared, runfile.Path
	}
	if len(detected) > 0 {
		return detected, "detected from this work tree"
	}
	return nil, ""
}

func devSetupValue(cmd []string) string {
	if len(cmd) == 0 {
		return "(none)"
	}
	return strings.Join(cmd, " ")
}

func devFromLabel(from string) string {
	if from == "" {
		return ""
	}
	return "   (" + from + ")"
}

func devSetupPortAdvice() string {
	port, _ := runtime.LocalDevPort("")
	if !runtime.LocalPortListening(port) {
		return fmt.Sprintf("Port %d is free on this machine, so the dev command's\n"+
			"default binding works and your branch is on http://localhost:%d.",
			port, port)
	}
	return fmt.Sprintf("Port %d is already taken here — the session's own dev\n"+
		"server runs on this machine. Start yours on a different port. The rule that the\n"+
		"dev server binds %d governs the SESSION's dev server, which the network splice\n"+
		"dials; it does not govern a private run of your own. Do not commit the change.",
		port, port)
}
