package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/valthaide/conops-private/internal/api"
)

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Get resources",
}

var getAppsCmd = &cobra.Command{
	Use:   "apps",
	Short: "List applications",
	RunE: func(cmd *cobra.Command, args []string) error {
		apps, err := listApps(NewClient())
		if err != nil {
			return err
		}
		if outputFormat() == "json" {
			return printJSON(apps)
		}
		printAppsTable(apps)
		return nil
	},
}

var getAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Show application details and runtime containers",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		containers, _ := listContainers(client, app.ID)

		if outputFormat() == "json" {
			return printJSON(map[string]interface{}{
				"app":        app,
				"containers": containers,
			})
		}
		printAppDetail(*app)
		fmt.Println()
		printContainersTable(containers)
		return nil
	},
}

var getControllerCmd = &cobra.Command{
	Use:   "controller",
	Short: "Show controller health and version",
	RunE: func(cmd *cobra.Command, args []string) error {
		return printControllerStatus()
	},
}

func listContainers(c *APIClient, appID string) ([]api.Container, error) {
	resp, err := c.Get("/api/v1/apps/" + appID + "/containers")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	var apiResp struct {
		Data []api.Container `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}
	if apiResp.Data == nil {
		return []api.Container{}, nil
	}
	return apiResp.Data, nil
}

func printContainersTable(containers []api.Container) {
	fmt.Println("Containers:")
	if len(containers) == 0 {
		fmt.Println("  (none)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tSTATUS\tHEALTH\tIMAGE\tPORTS\tNAME")
	for _, c := range containers {
		health := c.Health
		if health == "" {
			health = "-"
		}
		ports := c.Ports
		if ports == "" {
			ports = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Service, c.Status, health, c.Image, ports, c.Name)
	}
	_ = w.Flush()
}

func init() {
	getCmd.AddCommand(getAppsCmd, getAppCmd, getControllerCmd)
	rootCmd.AddCommand(getCmd)

	getCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("resource required: apps|app|controller")
	}
}
