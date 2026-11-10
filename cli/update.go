package cli

import (
	"github.com/nwylynko/clubhouse-cli/update"
	"github.com/spf13/cobra"
)

func runUpdate(cmd *cobra.Command, _ []string) error {
	dir, err := update.BinaryDir()
	if err != nil {
		return err
	}
	return update.Apply(cmd.Context(), cmd.OutOrStdout(), dir)
}
