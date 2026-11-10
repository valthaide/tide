package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/api"
)

var versionClientOnly bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print tidectl and controller versions",
	Run: func(cmd *cobra.Command, args []string) {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "tidectl %s\n", Version)
		if versionClientOnly {
			return
		}
		client := NewClient()
		resp, err := client.Get("/healthz")
		if err != nil {
			fmt.Fprintln(out, "controller unavailable")
			return
		}
		defer resp.Body.Close()
		var health api.HealthStatus
		if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
			return
		}
		fmt.Fprintf(out, "controller %s\n", health.Version)
		if health.Version != "" && health.Version != Version {
			fmt.Fprintf(out, "warning: tidectl %s and controller %s differ\n", Version, health.Version)
		}
	},
}

func init() {
	versionCmd.Flags().BoolVar(&versionClientOnly, "client", false, "Print client version only without contacting controller")
	rootCmd.AddCommand(versionCmd)
}
