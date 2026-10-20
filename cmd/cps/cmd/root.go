package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Version is set by GoReleaser ldflags.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:           "cps",
	Short:         "CLI for the ConOps controller",
	Long:          "Flux-style CLI for ConOps: inspect apps, create/delete, reconcile against the controller API.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().String("url", "", "Controller URL (default from cps config or https://127.0.0.1:8443)")
	rootCmd.PersistentFlags().String("token", "", "API bearer token (or CONOPS_TOKEN / cps config)")
	rootCmd.PersistentFlags().String("context", "", "cps config context name")
	rootCmd.PersistentFlags().String("config", "", "Path to cps config file (default ~/.conops/config)")
	rootCmd.PersistentFlags().Bool("insecure", false, "Skip TLS certificate verification")
	rootCmd.PersistentFlags().String("certificate-authority", "", "Path to CA certificate for HTTPS")
	rootCmd.PersistentFlags().StringP("output", "o", "table", "Output format: table|json")
	_ = viper.BindPFlag("url", rootCmd.PersistentFlags().Lookup("url"))
	_ = viper.BindPFlag("token", rootCmd.PersistentFlags().Lookup("token"))
	_ = viper.BindPFlag("context", rootCmd.PersistentFlags().Lookup("context"))
	_ = viper.BindPFlag("config", rootCmd.PersistentFlags().Lookup("config"))
	_ = viper.BindPFlag("insecure", rootCmd.PersistentFlags().Lookup("insecure"))
	_ = viper.BindPFlag("certificate-authority", rootCmd.PersistentFlags().Lookup("certificate-authority"))
	_ = viper.BindPFlag("output", rootCmd.PersistentFlags().Lookup("output"))
}

func initConfig() {
	viper.SetEnvPrefix("CONOPS")
	viper.AutomaticEnv()
	_ = viper.BindEnv("token", "CONOPS_TOKEN", "CONOPS_API_TOKEN")
}
