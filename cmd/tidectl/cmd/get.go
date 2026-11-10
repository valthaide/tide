package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/api"
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
		if outputFormat() == "wide" {
			printAppsTable(apps)
			for _, app := range apps {
				if app.Oneshot != "" {
					fmt.Printf("  %s oneshot %s\n", app.Name, app.Oneshot)
				}
			}
			return nil
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
	color := colorEnabled()
	headers := []string{"SERVICE", "STATUS", "HEALTH", "IMAGE", "PORTS", "NAME"}
	rows := make([][]string, 0, len(containers))
	for _, c := range containers {
		health := c.Health
		if health == "" {
			health = "-"
		}
		ports := c.Ports
		if ports == "" {
			ports = "-"
		}
		rows = append(rows, []string{c.Service, c.Status, health, c.Image, ports, c.Name})
	}
	printAlignedTable(headers, rows,
		func(_ int, plain string) string { return colorHeader(color, plain) },
		func(_ int, plain string) string { return colorData(color, plain) },
	)
}

var getQueueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Show the apply queue",
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		resp, err := client.Get("/api/v1/queue")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if err := CheckResponse(resp); err != nil {
			return err
		}
		var apiResp struct {
			Data []struct {
				Name     string    `json:"app"`
				Trigger  string    `json:"trigger"`
				Class    int       `json:"class"`
				Priority int       `json:"priority"`
				QueuedAt time.Time `json:"queued_at"`
				Reasons  []string  `json:"reasons"`
				Running  bool      `json:"running"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return err
		}
		if outputFormat() == "json" {
			return printJSON(apiResp.Data)
		}
		headers := []string{"POS", "APP", "TRIGGER", "CLASS", "PRIORITY", "QUEUED", "REASONS"}
		rows := make([][]string, 0, len(apiResp.Data))
		for i, item := range apiResp.Data {
			state := strings.Join(item.Reasons, ",")
			if item.Running {
				state = "running " + state
			}
			rows = append(rows, []string{
				fmt.Sprintf("%d", i+1),
				item.Name,
				orDash(item.Trigger),
				fmt.Sprintf("%d", item.Class),
				fmt.Sprintf("%d", item.Priority),
				relativeTime(item.QueuedAt),
				orDash(state),
			})
		}
		color := colorEnabled()
		printAlignedTable(headers, rows,
			func(_ int, plain string) string { return colorHeader(color, plain) },
			func(_ int, plain string) string { return colorData(color, plain) },
		)
		return nil
	},
}

func init() {
	getCmd.AddCommand(getAppsCmd, getAppCmd, getControllerCmd, getQueueCmd)
	rootCmd.AddCommand(getCmd)

	getCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("resource required: apps|app|controller|queue")
	}
}
