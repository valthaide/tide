package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/valthaide/conops-private/internal/api"
)

var (
	reconcileWait    bool
	reconcileTimeout time.Duration
	reconcileSync    bool
	reconcileForce   bool
)

var reconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Trigger reconciliation",
}

var reconcileAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Reconcile an application (async by default)",
	Long: `Triggers POST /api/v1/apps/{id}/reconcile and prints the operation_id.

With --wait, polls GET /api/v1/operations/{id} until terminal state.
Exit codes with --wait: 0 ok, 2 failed, 3 timeout.

--sync uses the legacy synchronous POST /sync endpoint.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		app, err := resolveApp(client, args[0])
		if err != nil {
			return err
		}

		if reconcileSync {
			resp, err := client.Post("/api/v1/apps/"+app.ID+"/sync", nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return CheckResponse(resp)
			}
			fmt.Printf("reconcile completed for %s (%s)\n", app.Name, app.ID)
			return nil
		}

		path := "/api/v1/apps/" + app.ID + "/reconcile"
		if reconcileForce {
			path += "?force=true"
		}
		resp, err := client.Post(path, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
		}

		var accepted struct {
			Message     string         `json:"message"`
			OperationID string         `json:"operation_id"`
			Data        *api.Operation `json:"data"`
		}
		if err := json.Unmarshal(body, &accepted); err != nil {
			return fmt.Errorf("decode reconcile response: %w", err)
		}
		opID := accepted.OperationID
		if opID == "" && accepted.Data != nil {
			opID = accepted.Data.ID
		}
		if opID == "" {
			return fmt.Errorf("reconcile response missing operation_id")
		}
		var queued struct {
			Position int `json:"position"`
			Class    int `json:"class"`
		}
		_ = json.Unmarshal(body, &queued)
		if queued.Position > 0 {
			fmt.Printf("queued %s position %d class %d\n", app.Name, queued.Position, queued.Class)
		}
		fmt.Printf("reconcile accepted for %s (%s) operation=%s\n", app.Name, app.ID, opID)

		if !reconcileWait {
			return nil
		}

		deadline := time.Now().Add(reconcileTimeout)
		for {
			op, err := getOperation(client, opID)
			if err != nil {
				return err
			}
			switch op.Status {
			case "succeeded":
				fmt.Printf("reconcile succeeded for %s\n", app.Name)
				return nil
			case "failed":
				msg := op.Message
				if op.Code != "" {
					msg = fmt.Sprintf("[%s/%s] %s", op.Code, op.Stage, op.Message)
				}
				fmt.Fprintf(os.Stderr, "reconcile failed for %s: %s\n", app.Name, msg)
				os.Exit(2)
				return nil
			}
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "reconcile timed out for %s (operation=%s status=%s)\n", app.Name, opID, op.Status)
				os.Exit(3)
				return nil
			}
			time.Sleep(2 * time.Second)
		}
	},
}

func getOperation(c *APIClient, id string) (*api.Operation, error) {
	resp, err := c.Get("/api/v1/operations/" + id)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	var apiResp struct {
		Data api.Operation `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode operation: %w", err)
	}
	if apiResp.Data.ID == "" {
		return nil, fmt.Errorf("empty operation")
	}
	return &apiResp.Data, nil
}

var reconcileAllCmd = &cobra.Command{
	Use:   "all",
	Short: "Enqueue every application that is not suspended",
	RunE: func(cmd *cobra.Command, args []string) error {
		client := NewClient()
		apps, err := listApps(client)
		if err != nil {
			return err
		}
		for _, app := range apps {
			if app.Suspended {
				continue
			}
			path := "/api/v1/apps/" + app.ID + "/reconcile"
			if reconcileForce {
				path += "?force=true"
			}
			resp, err := client.Post(path, nil)
			if err != nil {
				return err
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
				return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
			}
			fmt.Printf("queued %s\n", app.Name)
		}
		return nil
	},
}

func init() {
	reconcileAppCmd.Flags().BoolVar(&reconcileWait, "wait", false, "Wait for the reconcile operation to finish")
	reconcileAppCmd.Flags().DurationVar(&reconcileTimeout, "timeout", 5*time.Minute, "Timeout for --wait")
	reconcileAppCmd.Flags().BoolVar(&reconcileSync, "sync", false, "Use legacy synchronous POST /sync")
	reconcileAppCmd.Flags().BoolVar(&reconcileForce, "force", false, "Bypass the CI success gate")
	reconcileAllCmd.Flags().BoolVar(&reconcileForce, "force", false, "Bypass the CI success gate")
	reconcileCmd.AddCommand(reconcileAppCmd, reconcileAllCmd)
	rootCmd.AddCommand(reconcileCmd)
}
