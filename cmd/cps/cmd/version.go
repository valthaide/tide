package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/valthaide/conops-private/internal/api"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print cps version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("cps %s\n", Version)
		client := NewClient()
		resp, err := client.Get("/healthz")
		if err != nil {
			fmt.Println("controller unavailable")
			return
		}
		defer resp.Body.Close()
		var health api.HealthStatus
		if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
			return
		}
		fmt.Printf("controller %s\n", health.Version)
		if health.Version != "" && health.Version != Version {
			fmt.Printf("warning: cps %s and controller %s differ\n", Version, health.Version)
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
