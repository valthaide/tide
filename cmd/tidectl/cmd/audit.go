package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/api"
)

var auditLimit int

var auditCmd = &cobra.Command{
	Use:   "audit [APP]",
	Short: "Show persistent security and repository migration audit events",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		path := "/api/v1/audit?limit=" + strconv.Itoa(auditLimit)
		if len(args) == 1 {
			app, err := resolveApp(client, args[0])
			if err != nil {
				return err
			}
			path = "/api/v1/apps/" + app.ID + "/audit?limit=" + strconv.Itoa(auditLimit)
		}

		resp, err := client.Get(path)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if err := CheckResponse(resp); err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var apiResp struct {
			Data []api.AuditEvent `json:"data"`
		}
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return fmt.Errorf("decode audit events: %w", err)
		}
		if outputFormat() == "json" {
			return printJSON(apiResp.Data)
		}
		if len(apiResp.Data) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No audit events")
			return nil
		}
		headers := []string{"TIME", "KIND", "ACTOR", "APP", "DECISION", "SHA", "PIPELINE", "REASON"}
		rows := make([][]string, 0, len(apiResp.Data))
		for _, ev := range apiResp.Data {
			pipe := "-"
			if ev.PipelineID > 0 {
				pipe = fmt.Sprintf("%d (%s/%s)", ev.PipelineID, orDash(ev.PipelineSource), orDash(ev.PipelineStatus))
			}
			rows = append(rows, []string{
				ev.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
				ev.Kind,
				orDash(ev.Actor),
				orDash(ev.AppName),
				ev.Decision,
				orDash(shortCommit(ev.GitSHA)),
				pipe,
				truncate(orDash(ev.Reason), 48),
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
	auditCmd.Flags().IntVar(&auditLimit, "limit", 50, "Maximum audit events to return")
	rootCmd.AddCommand(auditCmd)
}
