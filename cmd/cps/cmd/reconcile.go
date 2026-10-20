package cmd

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

var reconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Trigger reconciliation",
}

var reconcileAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Force sync an application now",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		resp, err := client.Post("/api/v1/apps/"+app.ID+"/sync", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return CheckResponse(resp)
		}
		fmt.Printf("reconcile triggered for %s (%s)\n", app.Name, app.ID)
		return nil
	},
}

func init() {
	reconcileCmd.AddCommand(reconcileAppCmd)
	rootCmd.AddCommand(reconcileCmd)
}
