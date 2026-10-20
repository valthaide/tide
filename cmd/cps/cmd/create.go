package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/valthaide/conops-private/internal/api"
)

var createFile string

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create resources",
}

var createAppCmd = &cobra.Command{
	Use:   "app",
	Short: "Register an application from a JSON file",
	Example: `  cps create app -f app.json
  cps create app -f app.json -o json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(createFile) == "" {
			return fmt.Errorf("--file is required")
		}
		data, err := os.ReadFile(createFile)
		if err != nil {
			return err
		}
		var body map[string]interface{}
		if err := json.Unmarshal(data, &body); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}

		client := NewClient()
		resp, err := client.Post("/api/v1/apps/", body)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			return CheckResponse(resp)
		}

		var apiResp struct {
			Message string  `json:"message"`
			Data    api.App `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		if outputFormat() == "json" {
			return printJSON(apiResp.Data)
		}
		fmt.Println(apiResp.Message)
		if apiResp.Data.ID != "" {
			fmt.Printf("id: %s\n", apiResp.Data.ID)
		}
		if apiResp.Data.Name != "" {
			fmt.Printf("name: %s\n", apiResp.Data.Name)
		}
		return nil
	},
}

func init() {
	createAppCmd.Flags().StringVarP(&createFile, "file", "f", "", "Path to app JSON manifest")
	_ = createAppCmd.MarkFlagRequired("file")
	createCmd.AddCommand(createAppCmd)
	rootCmd.AddCommand(createCmd)
}
