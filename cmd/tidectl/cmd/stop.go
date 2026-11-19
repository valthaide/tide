package cmd

import (
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop APP",
	Short: "Stop all long-running services of an app and pause its reconciliation",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return appAction(args[0], "stop", "stopped")
	},
}

var startCmd = &cobra.Command{
	Use:   "start APP",
	Short: "Start a stopped app and resume its reconciliation",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return appAction(args[0], "start", "started")
	},
}

func appAction(ref, action, done string) error {
	client := NewClient()
	app, err := resolveApp(client, ref)
	if err != nil {
		return err
	}
	resp, err := client.Post("/api/v1/apps/"+app.ID+"/"+action, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}
	fmt.Printf("%s %s\n", done, app.Name)
	return nil
}

func init() {
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(startCmd)
}
