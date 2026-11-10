package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/api"
)

var (
	patchName         string
	patchBranch       string
	patchComposePath  string
	patchPollInterval string
)

var patchCmd = &cobra.Command{
	Use:   "patch",
	Short: "Patch resources",
}

var patchAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Update application fields",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		updates := map[string]interface{}{}
		if cmd.Flags().Changed("name") {
			updates["name"] = patchName
		}
		if cmd.Flags().Changed("branch") {
			updates["branch"] = patchBranch
		}
		if cmd.Flags().Changed("compose-path") {
			updates["compose_path"] = patchComposePath
		}
		if cmd.Flags().Changed("poll-interval") {
			updates["poll_interval"] = patchPollInterval
		}
		if len(updates) == 0 {
			return fmt.Errorf("no fields to patch")
		}

		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		resp, err := client.Patch("/api/v1/apps/"+app.ID, updates)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return CheckResponse(resp)
		}

		var apiResp struct {
			Message string  `json:"message"`
			Data    api.App `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		if outputFormat() == "json" {
			return printJSON(apiResp.Data)
		}
		name := apiResp.Data.Name
		id := apiResp.Data.ID
		if name == "" {
			name = app.Name
		}
		if id == "" {
			id = app.ID
		}
		fmt.Printf("patched app %s (%s)\n", name, id)
		return nil
	},
}

func init() {
	patchAppCmd.Flags().StringVar(&patchName, "name", "", "New app name")
	patchAppCmd.Flags().StringVar(&patchBranch, "branch", "", "Git branch")
	patchAppCmd.Flags().StringVar(&patchComposePath, "compose-path", "", "Compose file path")
	patchAppCmd.Flags().StringVar(&patchPollInterval, "poll-interval", "", "Poll interval (e.g. 30s)")
	patchCmd.AddCommand(patchAppCmd)
	rootCmd.AddCommand(patchCmd)
}
