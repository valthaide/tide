package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/fleetcheck"
)

var (
	checkTemplateFleetDir string
	checkTemplateName     string
	checkTemplateRepoDir  string
	checkTemplateAppDir   string
	checkTemplateProject  string
)

// checkTemplateCmd renders and validates a fleet template merged with an
// app's compose.override.yaml, without a controller. It is meant for CI:
// the same file order and --project-directory a deploy would use, checked
// with `docker compose config` before anything is pulled or started.
var checkTemplateCmd = &cobra.Command{
	Use:   "check-template",
	Short: "Render and validate a fleet template with an app override (no controller)",
	Long: `Builds the compose invocation for a template-sourced application exactly
as a deploy would:

  docker compose --project-directory <repo-dir>/<app-dir> -p <project> \
    -f <fleet-dir>/templates/<template>/compose.yaml \
    [-f <repo-dir>/<app-dir>/compose.override.yaml] \
    config

--repo-dir is the application repository checkout and --app-dir its app
folder inside it (e.g. backend; default is the repository root). Without
--repo-dir, --app-dir names the app folder directly (previous behaviour).
The override is optional and silently skipped when absent; .env and
env_file resolve from the app folder. No controller, pull, or apply is
involved, so this is safe to run in CI for both the fleet repository
(template changes) and an application repository (override changes).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(checkTemplateName) == "" {
			return exitError{code: 2, err: fmt.Errorf("--template is required")}
		}
		if issue := fleetcheck.CheckTemplateName(checkTemplateName); issue != nil {
			return exitError{code: 2, err: fmt.Errorf("%s", issue.Message)}
		}
		if strings.TrimSpace(checkTemplateFleetDir) == "" {
			return exitError{code: 2, err: fmt.Errorf("--fleet-dir is required (fleet checkout holding templates/)")}
		}
		repoDir := strings.TrimSpace(checkTemplateRepoDir)
		appDirRel := ""
		if repoDir != "" {
			appDirRel = fleetcheck.NormalizeAppDir(checkTemplateAppDir)
			if issue := fleetcheck.CheckAppDir(appDirRel); issue != nil {
				return exitError{code: 2, err: fmt.Errorf("%s", issue.Message)}
			}
		} else {
			if strings.TrimSpace(checkTemplateAppDir) == "" {
				return exitError{code: 2, err: fmt.Errorf("--app-dir is required (app folder in its repository, or a folder with --repo-dir)")}
			}
			repoDir = checkTemplateAppDir
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		exec := compose.NewComposeExecutor(nil)
		inv, configJSON, err := exec.CheckTemplate(ctx, checkTemplateFleetDir, checkTemplateName, repoDir, appDirRel, checkTemplateProject, nil)
		if err != nil {
			return exitError{code: 1, err: err}
		}
		services := templateCheckServices(configJSON)

		if outputFormat() == "json" {
			return printJSON(map[string]interface{}{
				"ok":              true,
				"template":        inv.Template,
				"template_file":   inv.TemplateFile,
				"repo_dir":        repoDir,
				"app_dir":         inv.ProjectDir,
				"app_dir_rel":     inv.AppDir,
				"project":         orDash(checkTemplateProject),
				"has_override":    inv.HasOverride,
				"override_file":   inv.OverrideFile,
				"compose_files":   inv.Files,
				"services":        services,
				"rendered_config": string(configJSON),
			})
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "ok\n")
		fmt.Fprintf(out, "Template:       %s\n", inv.Template)
		fmt.Fprintf(out, "Template File:  %s\n", inv.TemplateFile)
		fmt.Fprintf(out, "App Dir:        %s\n", inv.ProjectDir)
		if inv.HasOverride {
			fmt.Fprintf(out, "Override:       %s\n", inv.OverrideFile)
		} else {
			fmt.Fprintf(out, "Override:       (none)\n")
		}
		fmt.Fprintf(out, "Services:       %s\n", strings.Join(services, ", "))
		return nil
	},
}

func templateCheckServices(configJSON []byte) []string {
	sum, err := fleetcheck.SummarizeCompose(configJSON)
	if err != nil || len(sum.Images) == 0 {
		// Summarize only indexes images; fall back to service names from images map keys.
		if err == nil {
			names := make([]string, 0, len(sum.Images))
			for name := range sum.Images {
				names = append(names, name)
			}
			sort.Strings(names)
			return names
		}
		return nil
	}
	names := make([]string, 0, len(sum.Images))
	for name := range sum.Images {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func init() {
	checkTemplateCmd.Flags().StringVar(&checkTemplateFleetDir, "fleet-dir", "", "Fleet checkout directory holding templates/<template>/compose.yaml")
	checkTemplateCmd.Flags().StringVar(&checkTemplateName, "template", "", "Template name (templates/<template>/compose.yaml)")
	checkTemplateCmd.Flags().StringVar(&checkTemplateRepoDir, "repo-dir", "", "Application repository checkout; --app-dir is then relative to it")
	checkTemplateCmd.Flags().StringVar(&checkTemplateAppDir, "app-dir", "", "App folder: a path (--repo-dir unset) or relative to --repo-dir; .env and compose.override.yaml live here")
	checkTemplateCmd.Flags().StringVar(&checkTemplateProject, "project", "", "Compose project name (default: derived from the app directory name)")
	rootCmd.AddCommand(checkTemplateCmd)
}
