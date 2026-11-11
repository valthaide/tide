package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/fleet"
)

var (
	validateFleetFile string
	validateAppsDir   string
	validateCompose   bool
	validateOffline   bool
)

type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e exitError) ExitCode() int { return e.code }

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Check a fleet host file without a controller",
	RunE: func(cmd *cobra.Command, args []string) error {
		if validateFleetFile == "" {
			return exitError{code: 2, err: fmt.Errorf("--fleet-file is required")}
		}
		data, err := os.ReadFile(validateFleetFile)
		if err != nil {
			return exitError{code: 2, err: err}
		}
		strict := envcompat.Getenv("TIDE_FLEET_STRICT")
		// The fleet checkout root holds templates/: the fleet file lives in
		// hosts/<host>.yaml, or directly at the root.
		fleetDir, _ := filepath.Abs(filepath.Dir(validateFleetFile))
		if filepath.Base(fleetDir) == "hosts" {
			fleetDir = filepath.Dir(fleetDir)
		}
		report, err := fleet.Validate(data, fleet.ValidateOptions{
			AppsDir:     validateAppsDir,
			WithCompose: validateCompose,
			Strict:      strict,
			FleetDir:    fleetDir,
		})
		if err != nil {
			return exitError{code: 2, err: err}
		}
		if outputFormat() == "json" {
			if err := printJSON(report); err != nil {
				return exitError{code: 2, err: err}
			}
		} else {
			for _, field := range report.Unknown {
				fmt.Printf("%s: %s: warning: unknown_field: %s\n", validateFleetFile, field, field)
			}
			for _, issue := range report.Issues {
				path := issue.Path
				if path == "" {
					path = "(document)"
				}
				fmt.Printf("%s: %s: %s\n", validateFleetFile, path, issue.Message)
			}
			if report.OK && len(report.Unknown) == 0 {
				fmt.Printf("%s: ok\n", validateFleetFile)
			}
		}
		if !report.OK {
			return exitError{code: 1, err: fmt.Errorf("validation failed")}
		}
		_ = validateOffline
		return nil
	},
}

func joinIssues(fields []string) string {
	out := ""
	for i, field := range fields {
		if i > 0 {
			out += ","
		}
		out += field
	}
	return out
}

func init() {
	validateCmd.Flags().StringVar(&validateFleetFile, "fleet-file", "", "Host fleet file")
	validateCmd.Flags().StringVar(&validateAppsDir, "apps-dir", "", "Directory with app compose files")
	validateCmd.Flags().BoolVar(&validateOffline, "offline", true, "Do not contact a controller or registry")
	validateCmd.Flags().BoolVar(&validateCompose, "with-compose", false, "Run docker compose config for image, port, and digest checks")
	rootCmd.AddCommand(validateCmd)
}
