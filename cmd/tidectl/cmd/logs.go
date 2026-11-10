package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var logsTail int

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show logs",
}

var logsAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Show last sync output for an application",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		app, err := resolveApp(NewClient(), args[0])
		if err != nil {
			return err
		}
		out := strings.TrimSpace(app.LastSyncOutput)
		if out == "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "(no sync output)")
			if errMsg := strings.TrimSpace(app.LastSyncError); errMsg != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "last_sync_error: %s\n", errMsg)
			}
			return nil
		}
		if logsTail > 0 {
			lines := strings.Split(out, "\n")
			if logsTail < len(lines) {
				lines = lines[len(lines)-logsTail:]
			}
			out = strings.Join(lines, "\n")
		}
		fmt.Println(out)
		return nil
	},
}

func init() {
	logsAppCmd.Flags().IntVar(&logsTail, "tail", 0, "Show only the last N lines (0 = all)")
	logsCmd.AddCommand(logsAppCmd)
	rootCmd.AddCommand(logsCmd)
}
