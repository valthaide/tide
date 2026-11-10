package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/api"
)

var (
	migrateRepoURL        string
	migrateRepoAuthMethod string
	migrateRepoKeyFile    string
	migrateBranch         string
	migrateRevision       string
	migrateComposePath    string
	migrateDryRun         bool
	migrateReason         string
)

var migrateRepoCmd = &cobra.Command{
	Use:   "migrate-repo APP",
	Short: "Safely change an application's Git repository without losing data",
	Long: `Validates the target repository, compose configuration, image pins, data mounts, and CI gate
BEFORE stopping the currently running stack. Volumes and compose project names are preserved.

Use --dry-run to inspect what will be stopped, kept, removed, and created without making changes.
Rollback by running the command again with the previous --repo-url.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(migrateRepoURL) == "" {
			return fmt.Errorf("--repo-url is required")
		}
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}

		req := api.RepoMigrationRequest{
			RepoURL:        strings.TrimSpace(migrateRepoURL),
			RepoAuthMethod: strings.TrimSpace(migrateRepoAuthMethod),
			Branch:         strings.TrimSpace(migrateBranch),
			Revision:       strings.TrimSpace(migrateRevision),
			ComposePath:    strings.TrimSpace(migrateComposePath),
			DryRun:         migrateDryRun,
			Reason:         strings.TrimSpace(migrateReason),
		}
		if strings.TrimSpace(migrateRepoKeyFile) != "" {
			keyBytes, readErr := readFileBytes(migrateRepoKeyFile)
			if readErr != nil {
				return readErr
			}
			req.DeployKey = string(keyBytes)
		}

		resp, err := client.Post("/api/v1/apps/"+app.ID+"/migrate-repo", req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("migration failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var apiResp struct {
			Message string                `json:"message"`
			Data    api.RepoMigrationPlan `json:"data"`
		}
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return fmt.Errorf("decode migration response: %w", err)
		}
		if outputFormat() == "json" {
			return printJSON(apiResp.Data)
		}

		p := apiResp.Data
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "%s\n", apiResp.Message)
		fmt.Fprintf(out, "App:             %s (%s)\n", p.AppName, p.AppID)
		fmt.Fprintf(out, "From Repo:       %s\n", p.OldRepoURL)
		fmt.Fprintf(out, "To Repo:         %s\n", p.NewRepoURL)
		fmt.Fprintf(out, "Target Commit:   %s\n", orDash(p.TargetCommit))
		fmt.Fprintf(out, "Compose Project: %s\n", p.ComposeProject)
		fmt.Fprintf(out, "CI Status:       %s\n", orDash(p.CIStatus))
		fmt.Fprintf(out, "Will Stop:       %s\n", joinOrNone(p.ToStop))
		fmt.Fprintf(out, "Kept Volumes:    %s\n", joinOrNone(p.KeptVolumes))
		fmt.Fprintf(out, "Will Remove:     %s\n", joinOrNone(p.ToRemove))
		fmt.Fprintf(out, "Will Create:     %s\n", joinOrNone(p.ToCreate))
		return nil
	},
}

func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

func init() {
	migrateRepoCmd.Flags().StringVar(&migrateRepoURL, "repo-url", "", "Target Git repository URL")
	migrateRepoCmd.Flags().StringVar(&migrateRepoAuthMethod, "auth-method", "", "Repository auth method (public|token|deploy_key)")
	migrateRepoCmd.Flags().StringVar(&migrateRepoKeyFile, "deploy-key-file", "", "Optional path to deploy key or token file")
	migrateRepoCmd.Flags().StringVar(&migrateBranch, "branch", "", "Target Git branch")
	migrateRepoCmd.Flags().StringVar(&migrateRevision, "revision", "", "Optional target tag, branch, or full SHA")
	migrateRepoCmd.Flags().StringVar(&migrateComposePath, "compose-path", "", "Compose file path inside target repository")
	migrateRepoCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false, "Validate and show migration plan without stopping or modifying anything")
	migrateRepoCmd.Flags().StringVar(&migrateReason, "reason", "", "Audit reason for the repository migration")
	rootCmd.AddCommand(migrateRepoCmd)
}
