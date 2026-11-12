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
	migrateTemplate       string
	migrateAppDir         string
	migrateAllowRestart   bool
	migrateAll            bool
	migrateDryRun         bool
	migrateReason         string
)

var migrateRepoCmd = &cobra.Command{
	Use:   "migrate-repo APP",
	Short: "Change an application's Git source without unnecessary container restarts",
	Long: `Validates the target repository/branch/compose source (compose_path, or a fleet
template with --template merged with the app's compose.override.yaml), CI gate, rendered compose configuration,
image pins, env_file, and data mounts BEFORE touching the currently running stack.

When rendered configuration matches the running containers (image+digest, env, ports, volumes,
networks, commands, labels), Tide adopts the new source and commit without restarting containers.
When configuration differs, Tide runs standard 'compose up' to recreate only changed services.
App ID, compose project name, volumes, and env_file are preserved.

Use --dry-run to inspect the migration mode, reason, and affected services without making changes.
Without --dry-run a plan that would recreate services is NOT applied unless --allow-restart is given.
With --all (and no APP) every registration on the host is dry-run and summarized, one line per app.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		if migrateAll {
			if len(args) > 0 {
				return fmt.Errorf("--all takes no APP argument")
			}
			return runMigrateAll(cmd, client)
		}
		if len(args) != 1 {
			return fmt.Errorf("APP is required (or use --all)")
		}
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		targetRepo := strings.TrimSpace(migrateRepoURL)
		if targetRepo == "" {
			targetRepo = strings.TrimSpace(app.RepoURL)
		}
		if targetRepo == "" {
			return fmt.Errorf("--repo-url is required")
		}

		req := api.RepoMigrationRequest{
			RepoURL:        targetRepo,
			RepoAuthMethod: strings.TrimSpace(migrateRepoAuthMethod),
			Branch:         strings.TrimSpace(migrateBranch),
			Revision:       strings.TrimSpace(migrateRevision),
			ComposePath:    strings.TrimSpace(migrateComposePath),
			Template:       strings.TrimSpace(migrateTemplate),
			DryRun:         migrateDryRun,
			AllowRestart:   migrateAllowRestart,
			Reason:         strings.TrimSpace(migrateReason),
		}
		if cmd.Flags().Changed("app-dir") {
			req.AppDir = strings.TrimSpace(migrateAppDir)
			req.AppDirSet = true
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
		fmt.Fprintf(out, "Branch:          %s\n", orDash(p.Branch))
		if p.Template != "" || p.OldTemplate != "" {
			fmt.Fprintf(out, "Template:        %s -> %s (override: %v)\n", orDash(p.OldTemplate), orDash(p.Template), p.HasOverride)
			fmt.Fprintf(out, "App Dir:         %s -> %s\n", orDash(p.OldAppDir), orDash(p.AppDir))
		} else {
			fmt.Fprintf(out, "Compose Path:    %s\n", orDash(p.ComposePath))
		}
		if p.Mode == "no_restart" {
			fmt.Fprintf(out, "Restart:         no_restart (rendered config identical; no container will be recreated)\n")
		}
		if len(p.DiffSummary) > 0 {
			fmt.Fprintf(out, "Diff:            %s\n", strings.Join(p.DiffSummary, "; "))
		}
		fmt.Fprintf(out, "Target Commit:   %s\n", orDash(p.TargetCommit))
		fmt.Fprintf(out, "Compose Project: %s\n", p.ComposeProject)
		fmt.Fprintf(out, "CI Status:       %s\n", orDash(p.CIStatus))
		fmt.Fprintf(out, "Migration Mode:  %s (%s)\n", orDash(p.Mode), orDash(p.Reason))
		if len(p.ChangedServices) > 0 {
			fmt.Fprintf(out, "Changed Svcs:    %s\n", strings.Join(p.ChangedServices, ", "))
		}
		if len(p.UnchangedServices) > 0 {
			fmt.Fprintf(out, "Unchanged Svcs:  %s\n", strings.Join(p.UnchangedServices, ", "))
		}
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

// batchPlan is one app's dry-run result in --all mode.
type batchPlan struct {
	App             string   `json:"app"`
	Mode            string   `json:"mode,omitempty"`
	Reason          string   `json:"reason,omitempty"`
	ChangedServices []string `json:"changed_services,omitempty"`
	Diff            []string `json:"diff_summary,omitempty"`
	Error           string   `json:"error,omitempty"`
}

// runMigrateAll dry-runs the same source migration for every registration
// on the host and prints a one-line summary per app. Nothing is applied:
// --all is a survey, so failures are reported per app and the exit code is
// non-zero when any app errors.
func runMigrateAll(cmd *cobra.Command, client *APIClient) error {
	if strings.TrimSpace(migrateTemplate) == "" && strings.TrimSpace(migrateComposePath) == "" && strings.TrimSpace(migrateRepoURL) == "" && strings.TrimSpace(migrateBranch) == "" {
		return fmt.Errorf("--all needs a target source, e.g. --template <name>")
	}
	apps, err := listApps(client)
	if err != nil {
		return err
	}
	results := make([]batchPlan, 0, len(apps))
	failures := 0
	for _, app := range apps {
		req := api.RepoMigrationRequest{
			RepoURL:        strings.TrimSpace(app.RepoURL),
			RepoAuthMethod: strings.TrimSpace(migrateRepoAuthMethod),
			Branch:         strings.TrimSpace(migrateBranch),
			Revision:       strings.TrimSpace(migrateRevision),
			ComposePath:    strings.TrimSpace(migrateComposePath),
			Template:       strings.TrimSpace(migrateTemplate),
			DryRun:         true,
			Reason:         strings.TrimSpace(migrateReason),
		}
		if cmd.Flags().Changed("repo-url") {
			req.RepoURL = strings.TrimSpace(migrateRepoURL)
		}
		if cmd.Flags().Changed("app-dir") {
			req.AppDir = strings.TrimSpace(migrateAppDir)
			req.AppDirSet = true
		}
		entry := batchPlan{App: app.Name}
		plan, migErr := postMigration(client, app.ID, req)
		if plan != nil {
			entry.Mode = plan.Mode
			entry.Reason = plan.Reason
			entry.ChangedServices = plan.ChangedServices
			entry.Diff = plan.DiffSummary
		}
		if migErr != nil {
			entry.Error = migErr.Error()
			failures++
		}
		results = append(results, entry)
	}
	if outputFormat() == "json" {
		if err := printJSON(map[string]interface{}{"dry_run": true, "apps": results}); err != nil {
			return err
		}
	} else {
		out := cmd.OutOrStdout()
		for _, r := range results {
			switch {
			case r.Error != "":
				fmt.Fprintf(out, "%s: error: %s\n", r.App, r.Error)
			case r.Mode == "no_restart":
				fmt.Fprintf(out, "%s: no_restart\n", r.App)
			default:
				fmt.Fprintf(out, "%s: %s (changed: %s)\n", r.App, orDash(r.Mode), strings.Join(r.ChangedServices, ", "))
				for _, d := range r.Diff {
					fmt.Fprintf(out, "  diff: %s\n", d)
				}
			}
		}
		fmt.Fprintf(out, "Summary: %d apps, %d no_restart, %d restarted, %d errors\n",
			len(results), countMode(results, "no_restart"), countMode(results, "restarted"), failures)
	}
	if failures > 0 {
		return exitError{code: 1, err: fmt.Errorf("%d app(s) failed dry-run", failures)}
	}
	return nil
}

func countMode(results []batchPlan, mode string) int {
	n := 0
	for _, r := range results {
		if r.Error == "" && r.Mode == mode {
			n++
		}
	}
	return n
}

// postMigration posts one migration request and decodes the plan from both
// success and error envelopes (the API returns the plan in `data` either way).
func postMigration(client *APIClient, appID string, req api.RepoMigrationRequest) (*api.RepoMigrationPlan, error) {
	resp, err := client.Post("/api/v1/apps/"+appID+"/migrate-repo", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var apiResp struct {
		Message string                `json:"message"`
		Data    api.RepoMigrationPlan `json:"data"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("decode migration response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := apiResp.Message
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return &apiResp.Data, fmt.Errorf("%s", msg)
	}
	return &apiResp.Data, nil
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
	migrateRepoCmd.Flags().StringVar(&migrateTemplate, "template", "", "Fleet template name (templates/<template>/compose.yaml); alternative to --compose-path")
	migrateRepoCmd.Flags().StringVar(&migrateAppDir, "app-dir", "", "App folder inside the repository for template mode (e.g. backend); empty means the repository root")
	migrateRepoCmd.Flags().BoolVar(&migrateAllowRestart, "allow-restart", false, "Apply even when the plan would recreate services (default: apply only no_restart plans)")
	migrateRepoCmd.Flags().BoolVar(&migrateAll, "all", false, "Dry-run the migration for every registration on the host and print a per-app summary")
	migrateRepoCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false, "Validate and show migration plan without stopping or modifying anything")
	migrateRepoCmd.Flags().StringVar(&migrateReason, "reason", "", "Audit reason for the repository migration")
	rootCmd.AddCommand(migrateRepoCmd)
}
