package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Get resources",
}

var getAppsCmd = &cobra.Command{
	Use:   "apps",
	Short: "List applications",
	RunE: func(cmd *cobra.Command, args []string) error {
		apps, err := listApps(NewClient())
		if err != nil {
			return err
		}
		if outputFormat() == "json" {
			return printJSON(apps)
		}
		printAppsTable(apps)
		return nil
	},
}

var getAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Show application details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		app, err := resolveApp(NewClient(), args[0])
		if err != nil {
			return err
		}
		if outputFormat() == "json" {
			return printJSON(app)
		}
		printAppDetail(*app)
		return nil
	},
}

func init() {
	getCmd.AddCommand(getAppsCmd, getAppCmd)
	rootCmd.AddCommand(getCmd)

	// Convenience: `cps get` without resource defaults to apps list help.
	getCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("resource required: apps|app")
	}
}
