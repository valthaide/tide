package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/valthaide/conops-private/internal/api"
)

var (
	waitTimeout   time.Duration
	waitInterval  time.Duration
	waitDigest    string
	waitRevision  string
	waitVarChecks []string
)

var waitCmd = &cobra.Command{
	Use:   "wait",
	Short: "Wait for resources",
}

var waitAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Wait until an application is ready",
	Long:  "Ready means status=synced, no sync error, no commit drift, and all containers running (and healthy when healthcheck exists).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		deadline := time.Now().Add(waitTimeout)
		var lastReason string

		for {
			app, err := resolveApp(client, args[0])
			if err != nil {
				return err
			}
			containers, cerr := listContainers(client, app.ID)
			ready, reason := appReady(*app, containers, cerr)
			if ready {
				if reason = waitMatches(*app); reason != "" {
					ready = false
				}
			}
			if app.Status == "failed" || strings.Contains(app.Status, "rolled_back") {
				return fmt.Errorf("%s %s: %s", app.Name, app.Status, app.LastSyncError)
			}
			if ready {
				fmt.Printf("%s is ready\n", app.Name)
				return nil
			}
			lastReason = reason
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for %s: %s", app.Name, lastReason)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "waiting: %s\n", reason)
			time.Sleep(waitInterval)
		}
	},
}

func appReady(app api.App, containers []api.Container, containersErr error) (bool, string) {
	if app.Status != "synced" {
		return false, fmt.Sprintf("status=%s", app.Status)
	}
	if strings.TrimSpace(app.LastSyncError) != "" {
		return false, "last_sync_error set"
	}
	if hasDrift(app) {
		return false, "commit drift"
	}
	if containersErr != nil {
		return false, "containers unavailable: " + containersErr.Error()
	}
	if len(containers) == 0 {
		return false, "no containers"
	}
	for _, c := range containers {
		if c.Status != "running" {
			return false, fmt.Sprintf("container %s status=%s", c.Service, c.Status)
		}
		if c.Health == "unhealthy" {
			return false, fmt.Sprintf("container %s unhealthy", c.Service)
		}
		if c.Health == "starting" {
			return false, fmt.Sprintf("container %s health starting", c.Service)
		}
	}
	return true, ""
}

func waitMatches(app api.App) string {
	if waitRevision != "" && app.LastSyncedCommit != waitRevision && !strings.HasPrefix(app.LastSyncedCommit, waitRevision) {
		return "revision mismatch"
	}
	if waitDigest != "" {
		found := false
		for _, img := range app.Images {
			if img.Digest == waitDigest || strings.Contains(img.Image, waitDigest) || strings.Contains(img.Digest, waitDigest) {
				found = true
			}
		}
		if !found {
			return "digest mismatch"
		}
	}
	for _, pair := range waitVarChecks {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || app.PublicVars[key] != value {
			return "vars mismatch " + pair
		}
	}
	return ""
}

func init() {
	waitAppCmd.Flags().DurationVar(&waitTimeout, "timeout", 5*time.Minute, "Maximum wait time")
	waitAppCmd.Flags().DurationVar(&waitInterval, "interval", 3*time.Second, "Poll interval")
	waitAppCmd.Flags().StringVar(&waitDigest, "digest", "", "Require a running image digest")
	waitAppCmd.Flags().StringVar(&waitRevision, "revision", "", "Require the synced git revision")
	waitAppCmd.Flags().StringArrayVar(&waitVarChecks, "vars", nil, "Require a public variable KEY=value")
	waitCmd.AddCommand(waitAppCmd)
	rootCmd.AddCommand(waitCmd)
}
