package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/valthaide/tide/internal/envcompat"
)

// Version is set by GoReleaser / build ldflags.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:           "tidectl",
	Short:         "CLI for the Tide controller",
	Long:          "GitOps CLI for Tide: inspect apps, create/delete, validate fleet files, and reconcile against the controller API.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() {
	code := ExecuteArgs(os.Args, os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

// ExecuteArgs runs the CLI with explicit argv and streams for unit testing.
func ExecuteArgs(argv []string, stdout, stderr io.Writer) int {
	if len(argv) > 0 {
		envcompat.CheckBinaryAlias(argv[0], stderr)
	}
	envcompat.SetCLIWarningWriter(stderr)
	v := Version
	if v == "" {
		v = "dev"
	}
	if len(argv) == 2 && (argv[1] == "--version" || argv[1] == "-v") {
		_, _ = fmt.Fprintf(stdout, "tidectl %s\n", v)
		return 0
	}
	if hFlag := rootCmd.Flags().Lookup("help"); hFlag != nil {
		_ = hFlag.Value.Set("false")
		hFlag.Changed = false
	}
	if vFlag := rootCmd.Flags().Lookup("version"); vFlag != nil {
		_ = vFlag.Value.Set("false")
		vFlag.Changed = false
	}
	rootCmd.Version = v
	rootCmd.SetVersionTemplate("tidectl {{.Version}}\n")
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	if len(argv) > 1 {
		rootCmd.SetArgs(argv[1:])
	} else {
		rootCmd.SetArgs([]string{})
	}
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		if coded, ok := err.(interface{ ExitCode() int }); ok && coded.ExitCode() != 0 {
			return coded.ExitCode()
		}
		return 1
	}
	return 0
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().String("url", "", "Controller URL (default from tidectl config or https://127.0.0.1:8443)")
	rootCmd.PersistentFlags().String("token", "", "DEPRECATED: visible in ps; use TIDE_TOKEN / TIDE_API_TOKEN or tidectl config")
	_ = rootCmd.PersistentFlags().MarkDeprecated("token", "visible in process list; prefer TIDE_TOKEN / TIDE_API_TOKEN or `tidectl config`")
	rootCmd.PersistentFlags().String("context", "", "tidectl config context name")
	rootCmd.PersistentFlags().String("config", "", "Path to tidectl config file (default ~/.tide/config)")
	rootCmd.PersistentFlags().Bool("insecure", false, "Skip TLS certificate verification")
	rootCmd.PersistentFlags().String("certificate-authority", "", "Path to CA certificate for HTTPS")
	rootCmd.PersistentFlags().StringP("output", "o", "table", "Output format: table|wide|json")
	_ = viper.BindPFlag("url", rootCmd.PersistentFlags().Lookup("url"))
	_ = viper.BindPFlag("token", rootCmd.PersistentFlags().Lookup("token"))
	_ = viper.BindPFlag("context", rootCmd.PersistentFlags().Lookup("context"))
	_ = viper.BindPFlag("config", rootCmd.PersistentFlags().Lookup("config"))
	_ = viper.BindPFlag("insecure", rootCmd.PersistentFlags().Lookup("insecure"))
	_ = viper.BindPFlag("certificate-authority", rootCmd.PersistentFlags().Lookup("certificate-authority"))
	_ = viper.BindPFlag("output", rootCmd.PersistentFlags().Lookup("output"))
}

func initConfig() {
	viper.SetEnvPrefix("TIDE")
	viper.AutomaticEnv()
}
