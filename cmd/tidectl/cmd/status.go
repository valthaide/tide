package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var statusTenant string

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show application and tenant status on the host",
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		apps, err := listApps(client)
		if err != nil {
			return err
		}
		filter := strings.TrimSpace(statusTenant)
		if filter != "" {
			filtered := apps[:0]
			for _, app := range apps {
				tenant := strings.TrimSpace(app.Tenant)
				if tenant == "" {
					tenant = app.Policy().Tenant
				}
				if strings.EqualFold(tenant, filter) {
					filtered = append(filtered, app)
				}
			}
			apps = filtered
		}
		if outputFormat() == "json" {
			payload := map[string]any{"apps": apps}
			if filter != "" {
				payload["tenant"] = filter
			}
			return printJSON(payload)
		}
		printAppsTable(apps)
		if filter != "" {
			fmt.Printf("\nTenant filter: %s\n", filter)
		}
		return nil
	},
}

var holdCmd = &cobra.Command{
	Use:   "hold TENANT",
	Short: "Manually freeze updates for a tenant",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reason, _ := cmd.Flags().GetString("reason")
		client := NewClient()
		resp, err := client.Post("/api/v1/tenants/"+args[0]+"/hold", map[string]string{"reason": reason})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return CheckResponse(resp)
		}
		if outputFormat() == "json" {
			return printJSONStream(resp)
		}
		fmt.Printf("held tenant %s\n", args[0])
		return nil
	},
}

var releaseCmd = &cobra.Command{
	Use:   "release TENANT",
	Short: "Release a tenant hold so updates can proceed",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		resp, err := client.Post("/api/v1/tenants/"+args[0]+"/release", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return CheckResponse(resp)
		}
		if outputFormat() == "json" {
			return printJSONStream(resp)
		}
		fmt.Printf("released tenant %s\n", args[0])
		return nil
	},
}

func printJSONStream(resp *http.Response) error {
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(raw)
}

func init() {
	statusCmd.Flags().StringVar(&statusTenant, "tenant", "", "Filter apps belonging to this tenant")
	holdCmd.Flags().String("reason", "", "Optional reason recorded with the hold")
	rootCmd.AddCommand(statusCmd, holdCmd, releaseCmd)
}
