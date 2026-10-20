package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export resources",
}

var exportAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Export application registration as JSON (no secrets)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		app, err := resolveApp(NewClient(), args[0])
		if err != nil {
			return err
		}
		manifest := map[string]interface{}{
			"name":             app.Name,
			"repo_url":         app.RepoURL,
			"repo_auth_method": app.RepoAuthMethod,
			"branch":           app.Branch,
			"compose_path":     app.ComposePath,
			"poll_interval":    app.PollInterval,
			"suspended":        app.Suspended,
		}
		// Always JSON for export (backup/transfer).
		if err := printJSON(manifest); err != nil {
			return err
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "# note: deploy_key / env secrets are not exported")
		return nil
	},
}

func init() {
	exportCmd.AddCommand(exportAppCmd)
	rootCmd.AddCommand(exportCmd)
}