package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/valthaide/conops-private/internal/imagegc"
)

var gcDryRun bool

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Remove unused ConOps images that are not needed for rollback",
	RunE: func(cmd *cobra.Command, args []string) error {
		path := "/api/v1/gc"
		if gcDryRun {
			path += "?dry_run=true"
		}
		resp, err := NewClient().Post(path, map[string]bool{"dry_run": gcDryRun})
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
			Data []imagegc.Image `json:"data"`
		}
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return err
		}
		if len(apiResp.Data) == 0 {
			fmt.Println("no images")
			return nil
		}
		var freed int64
		for _, image := range apiResp.Data {
			freed += image.Size
			fmt.Printf("%s %d\n", image.ID, image.Size)
		}
		fmt.Printf("freed_bytes %d\n", freed)
		return nil
	},
}

func init() {
	gcCmd.Flags().BoolVar(&gcDryRun, "dry-run", false, "List images that would be removed")
	rootCmd.AddCommand(gcCmd)
}
