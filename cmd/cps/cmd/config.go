package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var (
	configPathFlag string
	setContextName string
	setContextURL  string
	setContextToken string
	setContextInsecure bool
	setContextCA   string
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage cps contexts (kubeconfig-style)",
}

var configViewCmd = &cobra.Command{
	Use:   "view",
	Short: "Show the cps config file",
	RunE: func(cmd *cobra.Command, args []string) error {
		path := configFilePath()
		cfg, err := loadConfig(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "# %s\n", path)
		fmt.Fprintf(cmd.OutOrStdout(), "current-context: %s\n", cfg.CurrentContext)
		for name, ctx := range cfg.Contexts {
			token := ctx.Token
			if token != "" {
				token = "****"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "context %s:\n  url: %s\n  token: %s\n  insecure-skip-tls-verify: %v\n",
				name, ctx.URL, token, ctx.InsecureSkipTLSVerify)
			if ctx.CertificateAuthority != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  certificate-authority: %s\n", ctx.CertificateAuthority)
			}
		}
		return nil
	},
}

var configGetContextsCmd = &cobra.Command{
	Use:   "get-contexts",
	Short: "List contexts",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(configFilePath())
		if err != nil {
			return err
		}
		for name := range cfg.Contexts {
			mark := " "
			if name == cfg.CurrentContext {
				mark = "*"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", mark, name)
		}
		return nil
	},
}

var configCurrentContextCmd = &cobra.Command{
	Use:   "current-context",
	Short: "Show the current context name",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(configFilePath())
		if err != nil {
			return err
		}
		if cfg.CurrentContext == "" {
			return fmt.Errorf("no current context set")
		}
		fmt.Fprintln(cmd.OutOrStdout(), cfg.CurrentContext)
		return nil
	},
}

var configUseContextCmd = &cobra.Command{
	Use:   "use-context NAME",
	Short: "Set the current context",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := configFilePath()
		cfg, err := loadConfig(path)
		if err != nil {
			return err
		}
		name := strings.TrimSpace(args[0])
		if _, ok := cfg.Contexts[name]; !ok {
			return fmt.Errorf("context %q not found", name)
		}
		cfg.CurrentContext = name
		return saveConfig(path, cfg)
	},
}

var configSetContextCmd = &cobra.Command{
	Use:   "set-context",
	Short: "Create or update a context",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(setContextName)
		if name == "" {
			return fmt.Errorf("--name is required")
		}
		path := configFilePath()
		cfg, err := loadConfig(path)
		if err != nil {
			return err
		}
		ctx := cfg.Contexts[name]
		if setContextURL != "" {
			ctx.URL = setContextURL
		}
		if setContextToken != "" {
			ctx.Token = setContextToken
		}
		if cmd.Flags().Changed("insecure-skip-tls-verify") {
			ctx.InsecureSkipTLSVerify = setContextInsecure
		}
		if setContextCA != "" {
			ctx.CertificateAuthority = setContextCA
		}
		if ctx.URL == "" {
			return fmt.Errorf("url is required (pass --url)")
		}
		cfg.Contexts[name] = ctx
		if cfg.CurrentContext == "" {
			cfg.CurrentContext = name
		}
		return saveConfig(path, cfg)
	},
}

func configFilePath() string {
	if strings.TrimSpace(configPathFlag) != "" {
		return configPathFlag
	}
	return defaultConfigPath()
}

func init() {
	configCmd.PersistentFlags().StringVar(&configPathFlag, "config", "", "Path to cps config (default ~/.config/cps/config.yaml)")
	configSetContextCmd.Flags().StringVar(&setContextName, "name", "", "Context name")
	configSetContextCmd.Flags().StringVar(&setContextURL, "url", "", "Controller URL")
	configSetContextCmd.Flags().StringVar(&setContextToken, "token", "", "API bearer token")
	configSetContextCmd.Flags().BoolVar(&setContextInsecure, "insecure-skip-tls-verify", false, "Skip TLS certificate verification")
	configSetContextCmd.Flags().StringVar(&setContextCA, "certificate-authority", "", "Path to CA certificate")
	_ = configSetContextCmd.MarkFlagRequired("name")

	configCmd.AddCommand(configViewCmd, configGetContextsCmd, configCurrentContextCmd, configUseContextCmd, configSetContextCmd)
	rootCmd.AddCommand(configCmd)
}
