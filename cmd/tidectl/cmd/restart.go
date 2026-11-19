package cmd

import (
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart APP SERVICE",
	Short: "Restart one compose service without SSH or a new commit",
	Long: `Posts to /api/v1/apps/{id}/restart.

The controller runs docker compose restart for that one service and leaves the rest
of the stack alone. Use this to bounce a stuck consumer or worker without logging
into the host and without pushing an empty commit.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		resp, err := client.Post("/api/v1/apps/"+app.ID+"/restart", map[string]string{"service": args[1]})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
		}
		fmt.Printf("restarted %s service %s\n", app.Name, args[1])
		return nil
	},
}

func init() {
	rootCmd.AddCommand(restartCmd)
}
