package cmd

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete resources",
}

var deleteAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Delete an application and its compose project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		resp, err := client.Delete("/api/v1/apps/" + app.ID)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return CheckResponse(resp)
		}
		fmt.Printf("deleted app %s (%s)\n", app.Name, app.ID)
		return nil
	},
}

func init() {
	deleteCmd.AddCommand(deleteAppCmd)
	rootCmd.AddCommand(deleteCmd)
}
