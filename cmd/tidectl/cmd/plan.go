package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/compose"
)

var (
	planHost     bool
	planFleetRef string
	planJSON     bool
)

var planCmd = &cobra.Command{
	Use:   "plan [APP]",
	Short: "Show what an apply would change without changing anything",
	Long: `Reads GET /api/v1/apps/{id}/plan.

Per service the plan shows create, recreate, restart, remove or no_change with
reasons (image or digest, changed environment key names — never values, command,
healthcheck, ports, volumes, labels, env_file, force, network namespace
dependents). It also lists volumes that would be created and watch_files
reload/restart actions.

With --fleet-ref the plan renders the fleet template at that not-yet-applied
fleet revision, so fleet CI can show the impact of a commit before rollout.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		if planHost {
			apps, err := listApps(client)
			if err != nil {
				return err
			}
			if len(apps) == 0 {
				fmt.Println("no apps")
				return nil
			}
			failed := 0
			for i := range apps {
				if err := printAppPlan(client, &apps[i]); err != nil {
					failed++
					fmt.Fprintf(os.Stderr, "%s: %v\n", apps[i].Name, err)
				}
			}
			if failed > 0 {
				return fmt.Errorf("plan failed for %d app(s)", failed)
			}
			return nil
		}
		if len(args) == 0 {
			return fmt.Errorf("app name or id is required (or use --host)")
		}
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		return printAppPlan(client, app)
	},
}

func init() {
	planCmd.Flags().BoolVar(&planHost, "host", false, "plan for every app on this controller")
	planCmd.Flags().StringVar(&planFleetRef, "fleet-ref", "", "render the fleet template at this ref (commit, branch, tag)")
	planCmd.Flags().BoolVar(&planJSON, "json", false, "print the raw plan JSON")
	rootCmd.AddCommand(planCmd)
}

func fetchAppPlan(client *APIClient, appID string) (*compose.AppPlan, error) {
	path := "/api/v1/apps/" + appID + "/plan"
	if strings.TrimSpace(planFleetRef) != "" {
		path += "?fleet_ref=" + url.QueryEscape(strings.TrimSpace(planFleetRef))
	}
	resp, err := client.Get(path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	var apiResp struct {
		Data *compose.AppPlan `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if apiResp.Data == nil {
		return nil, fmt.Errorf("empty plan response")
	}
	return apiResp.Data, nil
}

func printAppPlan(client *APIClient, app *api.App) error {
	plan, err := fetchAppPlan(client, app.ID)
	if err != nil {
		return err
	}
	if planJSON {
		data, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("%s (project %s)\n", app.Name, plan.Project)
	var rows [][]string
	for _, s := range plan.Services {
		rows = append(rows, []string{s.Service, s.Action, strings.Join(s.Reasons, "; ")})
	}
	if len(rows) == 0 {
		fmt.Println("  no services")
	} else {
		printAlignedTable([]string{"SERVICE", "ACTION", "REASONS"}, rows, nil, nil)
	}
	if plan.ForceRecreateAll {
		fmt.Println("  ! env_file changed outside service mounts; the whole project is force-recreated")
	}
	for _, v := range plan.VolumesToCreate {
		kind := "volume"
		if v.External {
			kind = "external volume"
		}
		fmt.Printf("  + %s to create: %s\n", kind, v.Name)
	}
	for _, w := range plan.WatchActions {
		target := w.Service
		if target == "" {
			target = "(project)"
		}
		fmt.Printf("  ~ watch %s: %s -> %s\n", w.Action, w.Path, target)
	}
	return nil
}
