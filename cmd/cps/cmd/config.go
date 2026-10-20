package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var (
	configPathFlag         string
	setContextName         string
	setContextURL          string
	setContextToken        string
	setContextInsecure     bool
	setContextCA           string
	initFromSecrets        string
	initContextName        string
	initContextURL         string
	initInsecure           bool
	initSetCurrent         bool
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

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create ~/.conops/config from /etc/conops/secrets.env (or --from-secrets)",
	Long: `Create or update a cps context using CONOPS_API_TOKEN from a secrets env file.

Default secrets path: /etc/conops/secrets.env
Requires read permission on that file (e.g. sudo cps config init, or group-readable secrets).

Examples:
  sudo cps config init
  cps config init --from-secrets /etc/conops/secrets.env --name local
  cps config init --url https://127.0.0.1:8443 --insecure`,
	RunE: func(cmd *cobra.Command, args []string) error {
		secretsPath := strings.TrimSpace(initFromSecrets)
		if secretsPath == "" {
			secretsPath = "/etc/conops/secrets.env"
		}

		env, err := parseEnvFile(secretsPath)
		if err != nil {
			if os.IsPermission(err) {
				return fmt.Errorf("cannot read %s: permission denied (try sudo cps config init)", secretsPath)
			}
			if os.IsNotExist(err) {
				return fmt.Errorf("secrets file not found: %s", secretsPath)
			}
			return fmt.Errorf("read %s: %w", secretsPath, err)
		}

		token := strings.TrimSpace(env["CONOPS_API_TOKEN"])
		if token == "" {
			return fmt.Errorf("%s has no CONOPS_API_TOKEN", secretsPath)
		}

		name := strings.TrimSpace(initContextName)
		if name == "" {
			if h, herr := os.Hostname(); herr == nil && strings.TrimSpace(h) != "" {
				name = strings.TrimSpace(h)
			} else {
				name = "local"
			}
		}

		url := strings.TrimSpace(initContextURL)
		if url == "" {
			url = "https://127.0.0.1:8443"
		}

		path := configFilePath()
		// Prefer writing to ~/.conops/config even if legacy path was detected for reading.
		if strings.TrimSpace(configPathFlag) == "" {
			if home, herr := os.UserHomeDir(); herr == nil {
				path = filepath.Join(home, ".conops", "config")
			}
		}

		cfg, err := loadConfig(path)
		if err != nil {
			return err
		}

		ctx := cfg.Contexts[name]
		ctx.URL = url
		ctx.Token = token
		if cmd.Flags().Changed("insecure") {
			ctx.InsecureSkipTLSVerify = initInsecure
		} else if strings.Contains(url, "127.0.0.1") || strings.Contains(url, "localhost") {
			ctx.InsecureSkipTLSVerify = true
		}
		cfg.Contexts[name] = ctx
		if initSetCurrent || cfg.CurrentContext == "" {
			cfg.CurrentContext = name
		}

		if err := saveConfig(path, cfg); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (context %q)\n", path, name)
		return nil
	},
}

func configFilePath() string {
	if strings.TrimSpace(configPathFlag) != "" {
		return configPathFlag
	}
	return defaultConfigPath()
}

func init() {
	configCmd.PersistentFlags().StringVar(&configPathFlag, "config", "", "Path to cps config (default ~/.conops/config)")
	configSetContextCmd.Flags().StringVar(&setContextName, "name", "", "Context name")
	configSetContextCmd.Flags().StringVar(&setContextURL, "url", "", "Controller URL")
	configSetContextCmd.Flags().StringVar(&setContextToken, "token", "", "API bearer token")
	configSetContextCmd.Flags().BoolVar(&setContextInsecure, "insecure-skip-tls-verify", false, "Skip TLS certificate verification")
	configSetContextCmd.Flags().StringVar(&setContextCA, "certificate-authority", "", "Path to CA certificate")
	_ = configSetContextCmd.MarkFlagRequired("name")

	configInitCmd.Flags().StringVar(&initFromSecrets, "from-secrets", "/etc/conops/secrets.env", "Path to secrets.env with CONOPS_API_TOKEN")
	configInitCmd.Flags().StringVar(&initContextName, "name", "", "Context name (default: hostname)")
	configInitCmd.Flags().StringVar(&initContextURL, "url", "https://127.0.0.1:8443", "Controller URL")
	configInitCmd.Flags().BoolVar(&initInsecure, "insecure", true, "Skip TLS certificate verification")
	configInitCmd.Flags().BoolVar(&initSetCurrent, "use", true, "Set as current-context")

	configCmd.AddCommand(configViewCmd, configGetContextsCmd, configCurrentContextCmd, configUseContextCmd, configSetContextCmd, configInitCmd)
	rootCmd.AddCommand(configCmd)
}
