package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
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
	url := client.BaseURL
	resp, err := client.Get("/healthz")
	if err != nil {
		return fmt.Errorf("controller unreachable at %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read /healthz: %w", err)
	}

	var health api.HealthStatus
	if err := json.Unmarshal(body, &health); err != nil {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 120 {
			snippet = snippet[:120] + "…"
		}
		return fmt.Errorf("decode /healthz (HTTP %d): %w; body=%q", resp.StatusCode, err, snippet)
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
