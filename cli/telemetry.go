package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nwylynko/clubhouse-cli/telemetry"
)

func newTelemetryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "telemetry",
		Short: "Show or set whether this machine sends telemetry (off by default)",
		Args:  cobra.NoArgs,
		RunE:  runTelemetryStatus,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "on",
			Short: "Record this machine's session activity and send it to the session's ingest",
			Args:  cobra.NoArgs,
			RunE:  func(c *cobra.Command, _ []string) error { return runTelemetrySet(c, true) },
		},
		&cobra.Command{
			Use:   "off",
			Short: "Send nothing from this machine",
			Args:  cobra.NoArgs,
			RunE:  func(c *cobra.Command, _ []string) error { return runTelemetrySet(c, false) },
		},
		&cobra.Command{
			Use:   "status",
			Short: "Say whether this machine sends telemetry, and why",
			Args:  cobra.NoArgs,
			RunE:  runTelemetryStatus,
		},
	)
	return cmd
}

func runTelemetryStatus(cmd *cobra.Command, _ []string) error {
	_, why := telemetry.Resolve()
	fmt.Fprintln(cmd.OutOrStdout(), why)
	return nil
}

func runTelemetrySet(cmd *cobra.Command, on bool) error {
	if err := telemetry.SetMode(on); err != nil {
		return fmt.Errorf("remember the telemetry setting: %w", err)
	}
	_, why := telemetry.Resolve()
	fmt.Fprintln(cmd.OutOrStdout(), why)
	if on {
		fmt.Fprintln(cmd.OutOrStdout(),
			"this machine now records what it does in a session — its log lines, its agent activity — and sends them to the ingest the session advertises")
	}
	return nil
}
