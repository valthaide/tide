package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/secrets"
)

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Inspect the OpenBao secrets provider (no values)",
}

var secretsStatusCmd = &cobra.Command{
	Use:   "status [app]",
	Short: "Show provider, paths, versions, staleness, and token TTL",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		url := "/api/v1/secrets/status"
		if len(args) == 1 {
			url += "?app=" + args[0]
		}
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("secrets status failed: %s", strings.TrimSpace(string(body)))
		}
		var wrap struct {
			Data secrets.Status `json:"data"`
		}
		if err := json.Unmarshal(body, &wrap); err != nil {
			return err
		}
		st := wrap.Data
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Provider:  %s\n", st.Provider)
		if st.Enabled {
			fmt.Fprintf(out, "Addr:      %s\n", st.Addr)
			fmt.Fprintf(out, "Auth:      %s\n", st.Auth)
			fmt.Fprintf(out, "Token TTL: %s\n", time.Duration(st.TokenTTLSeconds*float64(time.Second)).Round(time.Second))
		} else {
			fmt.Fprintln(out, "Provider is not active (no apps use it or it is not configured)")
		}
		if len(st.Apps) == 0 {
			fmt.Fprintln(out, "Apps:      (none)")
			return nil
		}
		fmt.Fprintln(out, "Apps:")
		for _, app := range st.Apps {
			stale := ""
			if app.Stale {
				stale = "  STALE (cached values)"
			}
			fmt.Fprintf(out, "  %s%s\n", app.App, stale)
			if app.File != "" {
				fmt.Fprintf(out, "    file:         %s\n", app.File)
			}
			for _, p := range app.Paths {
				fmt.Fprintf(out, "    path:         %s  version=%d\n", p.Path, p.Version)
			}
			if !app.LastSuccess.IsZero() {
				fmt.Fprintf(out, "    last success: %s\n", app.LastSuccess.Local().Format(time.DateTime))
			}
			if app.LastError != "" {
				fmt.Fprintf(out, "    last error:   %s\n", app.LastError)
			}
		}
		return nil
	},
}

func init() {
	secretsCmd.AddCommand(secretsStatusCmd)
	rootCmd.AddCommand(secretsCmd)
}
