package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/valthaide/conops-private/internal/api"
)

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Check controller health",
	RunE: func(cmd *cobra.Command, args []string) error {
		return printControllerStatus()
	},
}

func printControllerStatus() error {
	client := NewClient()
	url := viper.GetString("url")
	resp, err := client.Get("/healthz")
	if err != nil {
		return fmt.Errorf("controller unreachable at %s: %w", url, err)
	}
	defer resp.Body.Close()

	var health api.HealthStatus
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return fmt.Errorf("decode /healthz: %w", err)
	}

	if outputFormat() == "json" {
		payload := map[string]interface{}{
			"url":     url,
			"status":  health.Status,
			"version": health.Version,
			"db":      health.DB,
			"http":    resp.StatusCode,
		}
		return printJSON(payload)
	}

	fmt.Printf("URL:     %s\n", url)
	fmt.Printf("Status:  %s\n", health.Status)
	fmt.Printf("Version: %s\n", health.Version)
	fmt.Printf("DB:      %s\n", health.DB)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("controller unhealthy (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(checkCmd)
}
