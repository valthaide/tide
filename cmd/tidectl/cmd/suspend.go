package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/api"
)

var suspendCmd = &cobra.Command{
	Use:   "suspend [APP]",
	Short: "Pause automatic reconciliation for one application",
	Long:  "While suspended, Tide does not poll git or reconcile this app. Other apps on the host keep syncing. The pause is stored locally and survives a controller restart; fleet git does not clear it. Resume with `tidectl resume`.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Help()
		}
		return setSuspended(args[0], true)
	},
}

var suspendAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Pause git polling and auto-sync for an application",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setSuspended(args[0], true)
	},
}

var resumeCmd = &cobra.Command{
	Use:   "resume [APP]",
	Short: "Resume automatic reconciliation for one application",
	Long:  "Clears an operator pause. The next reconcile applies the revision from git.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Help()
		}
		return setSuspended(args[0], false)
	},
}

var resumeAppCmd = &cobra.Command{
	Use:   "app NAME|ID",
	Short: "Resume git polling and auto-sync for an application",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setSuspended(args[0], false)
	},
}

func setSuspended(ref string, suspended bool) error {
	client := NewClient()
	app, err := resolveApp(client, ref)
	if err != nil {
		return err
	}
	path := "/api/v1/apps/" + app.ID + "/resume"
	action := "resumed"
	if suspended {
		path = "/api/v1/apps/" + app.ID + "/suspend"
		action = "suspended"
	}
	resp, err := client.Post(path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CheckResponse(resp)
	}
	var apiResp struct {
		Message string  `json:"message"`
		Data    api.App `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&apiResp)
	if outputFormat() == "json" {
		return printJSON(apiResp.Data)
	}
	name := apiResp.Data.Name
	if name == "" {
		name = app.Name
	}
	fmt.Printf("%s %s (%s)\n", action, name, app.ID)
	return nil
}

func init() {
	suspendCmd.AddCommand(suspendAppCmd)
	resumeCmd.AddCommand(resumeAppCmd)
	rootCmd.AddCommand(suspendCmd, resumeCmd)
}
