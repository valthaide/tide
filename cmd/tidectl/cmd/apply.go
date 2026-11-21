package cmd

import (
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

var applyCmd = &cobra.Command{
	Use:   "apply APP",
	Short: "Apply a pending fleet template change held by the rollout policy",
	Long: `Posts to /api/v1/apps/{id}/apply.

Under rollout: manual an app with a template change waits as held until this
command (or the API call) grants one apply. Under rollout: canary it lets one
app jump ahead of the staged order. Inspect the pending change first with
tidectl plan APP.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		resp, err := client.Post("/api/v1/apps/"+app.ID+"/apply", map[string]string{})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
		}
		fmt.Printf("apply queued for %s\n", app.Name)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(applyCmd)
}
