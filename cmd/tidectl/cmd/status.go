package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

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
				tenantName := strings.TrimSpace(app.Tenant)
				if tenantName == "" {
					tenantName = app.Policy().Tenant
				}
				if strings.EqualFold(tenantName, filter) {
					filtered = append(filtered, app)
				}
			}
			apps = filtered
		}

		var tenantsPayload any
		if holdInfo, herr := fetchTenantHolds(client, filter); herr == nil {
			tenantsPayload = holdInfo
			if filter != "" && outputFormat() != "json" {
				printTenantHold(filter, holdInfo)
			}
		}

		if outputFormat() == "json" {
			payload := map[string]any{"apps": apps}
			if filter != "" {
				payload["tenant"] = filter
			}
			if tenantsPayload != nil {
				payload["tenants"] = tenantsPayload
			}
			return printJSON(payload)
		}
		printAppsTable(apps)
		if filter != "" && tenantsPayload == nil {
			fmt.Printf("\nTenant filter: %s\n", filter)
		}
		return nil
	},
}

type tenantHoldRow struct {
	Name   string `json:"name"`
	Hold   *struct {
		Reason      string    `json:"reason"`
		Detail      string    `json:"detail,omitempty"`
		Since       time.Time `json:"since"`
		Manual      bool      `json:"manual"`
		HoldingApps []string  `json:"holding_apps,omitempty"`
		Stopped     []string  `json:"stopped,omitempty"`
	} `json:"hold,omitempty"`
	Apps []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"apps"`
	Policy string `json:"failure_policy,omitempty"`
}

func fetchTenantHolds(client *APIClient, filter string) ([]tenantHoldRow, error) {
	resp, err := client.Get("/api/v1/tenants")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, CheckResponse(resp)
	}
	var apiResp struct {
		Data []tenantHoldRow `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}
	if filter == "" {
		return apiResp.Data, nil
	}
	out := make([]tenantHoldRow, 0, 1)
	for _, row := range apiResp.Data {
		if strings.EqualFold(row.Name, filter) {
			out = append(out, row)
		}
	}
	return out, nil
}

func printTenantHold(filter string, rows []tenantHoldRow) {
	fmt.Printf("\nTenant: %s\n", filter)
	found := false
	for _, row := range rows {
		if !strings.EqualFold(row.Name, filter) {
			continue
		}
		found = true
		if row.Policy != "" {
			fmt.Printf("  failure_policy: %s\n", row.Policy)
		}
		if row.Hold == nil {
			fmt.Printf("  hold: none\n")
			continue
		}
		reason := row.Hold.Reason
		if reason == "" {
			if row.Hold.Manual {
				reason = "manual"
			} else {
				reason = "auto"
			}
		}
		fmt.Printf("  hold: %s", reason)
		if !row.Hold.Since.IsZero() {
			fmt.Printf(" since %s", row.Hold.Since.Format(time.RFC3339))
		}
		fmt.Println()
		if len(row.Hold.HoldingApps) > 0 {
			fmt.Printf("  holding_apps: %s\n", strings.Join(row.Hold.HoldingApps, ", "))
		}
		if row.Hold.Detail != "" {
			fmt.Printf("  detail: %s\n", row.Hold.Detail)
		}
	}
	if !found {
		fmt.Printf("  hold: none\n")
	}
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
	Long:  "Clears a manual hold until the next tidectl hold. For an automatic hold, clears it until the next new failure (not the next Evaluate while failures remain).",
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
