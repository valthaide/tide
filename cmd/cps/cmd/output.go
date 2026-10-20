package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/viper"
	"github.com/valthaide/conops-private/internal/api"
)

func outputFormat() string {
	return strings.ToLower(strings.TrimSpace(viper.GetString("output")))
}

func printJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func shortCommit(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}

func hasDrift(app api.App) bool {
	seen := strings.TrimSpace(app.LastSeenCommit)
	synced := strings.TrimSpace(app.LastSyncedCommit)
	return seen != "" && seen != synced
}

func readyValue(app api.App) string {
	if app.Status == "synced" && strings.TrimSpace(app.LastSyncError) == "" && !hasDrift(app) {
		return "True"
	}
	return "False"
}

func driftValue(app api.App) string {
	if hasDrift(app) {
		return "True"
	}
	return "False"
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func messageFor(app api.App) string {
	if errMsg := strings.TrimSpace(app.LastSyncError); errMsg != "" {
		return truncate(errMsg, 60)
	}
	if msg := strings.TrimSpace(app.LastSyncedCommitMessage); msg != "" {
		return truncate(msg, 60)
	}
	if msg := strings.TrimSpace(app.LastSeenCommitMessage); msg != "" {
		return truncate(msg, 60)
	}
	return "-"
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func printAppsTable(apps []api.App) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREADY\tSTATUS\tDRIFT\tREVISION\tLAST SYNC\tMESSAGE")
	for _, app := range apps {
		rev := shortCommit(app.LastSyncedCommit)
		if rev == "" {
			rev = shortCommit(app.LastSeenCommit)
		}
		if rev == "" {
			rev = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			app.Name,
			readyValue(app),
			app.Status,
			driftValue(app),
			rev,
			relativeTime(app.LastSyncAt),
			messageFor(app),
		)
	}
	_ = w.Flush()
}

func printAppDetail(app api.App) {
	fmt.Printf("Name:           %s\n", app.Name)
	fmt.Printf("ID:             %s\n", app.ID)
	fmt.Printf("Ready:          %s\n", readyValue(app))
	fmt.Printf("Status:         %s\n", app.Status)
	fmt.Printf("Drift:          %s\n", driftValue(app))
	fmt.Printf("Repo:           %s\n", app.RepoURL)
	fmt.Printf("Auth:           %s\n", app.RepoAuthMethod)
	fmt.Printf("Branch:         %s\n", app.Branch)
	fmt.Printf("Compose:        %s\n", app.ComposePath)
	fmt.Printf("Poll Interval:  %s\n", app.PollInterval)
	fmt.Printf("Seen Commit:    %s\n", orDash(app.LastSeenCommit))
	fmt.Printf("Seen Message:   %s\n", orDash(app.LastSeenCommitMessage))
	fmt.Printf("Synced Commit:  %s\n", orDash(app.LastSyncedCommit))
	fmt.Printf("Synced Message: %s\n", orDash(app.LastSyncedCommitMessage))
	fmt.Printf("Last Sync At:   %s\n", formatTime(app.LastSyncAt))
	fmt.Printf("Last Sync Err:  %s\n", orDash(app.LastSyncError))
	if out := strings.TrimSpace(app.LastSyncOutput); out != "" {
		fmt.Printf("\n--- Last Sync Output ---\n%s\n", out)
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return strings.TrimSpace(s)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339) + " (" + relativeTime(t) + ")"
}
