package cmd

import (
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

var retryCmd = &cobra.Command{
	Use:   "retry APP",
	Short: "Queue one application for sync without restarting ConOps",
	Long: `Posts to /api/v1/apps/{id}/retry.

The controller puts that app back on the apply queue and leaves every other app alone.
Use this when one app is stuck waiting and the rest of the host is healthy.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		resp, err := client.Post("/api/v1/apps/"+app.ID+"/retry", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
		}
		fmt.Printf("retry queued for %s (%s)\n", app.Name, app.ID)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(retryCmd)
}
