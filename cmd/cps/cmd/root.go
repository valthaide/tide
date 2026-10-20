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
	rootCmd.PersistentFlags().String("url", "http://127.0.0.1:8080", "Controller URL")
	rootCmd.PersistentFlags().StringP("output", "o", "table", "Output format: table|json")
	_ = viper.BindPFlag("url", rootCmd.PersistentFlags().Lookup("url"))
	_ = viper.BindPFlag("output", rootCmd.PersistentFlags().Lookup("output"))
}

func initConfig() {
	viper.SetEnvPrefix("CONOPS")
	viper.AutomaticEnv()
}
