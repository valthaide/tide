package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/valthaide/conops-private/internal/api"
)

var historyLimit int

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Show revision history",
}

var historyAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Show successful revisions for an application",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		resp, err := client.Get("/api/v1/apps/" + app.ID + "/history?limit=" + strconv.Itoa(historyLimit))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if err := CheckResponse(resp); err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var apiResp struct {
			Data []api.DeployEvent `json:"data"`
		}
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return fmt.Errorf("decode history: %w", err)
		}
		if outputFormat() == "json" {
			return printJSON(apiResp.Data)
		}
		if len(apiResp.Data) == 0 {
			fmt.Println("No revisions")
			return nil
		}
		for _, rev := range apiResp.Data {
			fmt.Printf("%s %s %s %s\n", rev.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), rev.Result, rev.GitSHA, rev.Images)
		}
		return nil
	},
}

func init() {
	historyAppCmd.Flags().IntVar(&historyLimit, "limit", 20, "Maximum revisions")
	historyCmd.AddCommand(historyAppCmd)
	rootCmd.AddCommand(historyCmd)
}
