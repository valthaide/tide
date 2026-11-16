package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/api"
)

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Inspect external env_files for an app",
}

var envStatusCmd = &cobra.Command{
	Use:   "status <app>",
	Short: "Show env file paths, hashes, and drift (no values)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}
		resp, err := client.Get("/api/v1/apps/" + app.ID + "/env")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("env status failed: %s", strings.TrimSpace(string(body)))
		}
		var wrap struct {
			Data api.EnvStatus `json:"data"`
		}
		if err := json.Unmarshal(body, &wrap); err != nil {
			return err
		}
		st := wrap.Data
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "App:           %s\n", st.AppName)
		fmt.Fprintf(out, "Current hash:  %s\n", orDash(st.CurrentHash))
		fmt.Fprintf(out, "Applied hash:  %s\n", orDash(st.AppliedHash))
		fmt.Fprintf(out, "Drift:         %v\n", st.Drift)
		if len(st.AppliedPaths) > 0 {
			fmt.Fprintf(out, "Applied paths: %s\n", strings.Join(st.AppliedPaths, ", "))
		}
		if len(st.Files) == 0 {
			fmt.Fprintln(out, "Files:         (none)")
			return nil
		}
		fmt.Fprintln(out, "Files:")
		for _, f := range st.Files {
			if f.Error != "" {
				fmt.Fprintf(out, "  %s  error=%s\n", f.Path, f.Error)
				continue
			}
			fmt.Fprintf(out, "  %s  sha256=%s\n", f.Path, f.SHA256)
		}
		return nil
	},
}

func init() {
	envCmd.AddCommand(envStatusCmd)
	rootCmd.AddCommand(envCmd)
}
