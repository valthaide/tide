package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// cpsConfig is the on-disk kubeconfig-like file for cps.
type cpsConfig struct {
	CurrentContext string                 `yaml:"current-context"`
	Contexts       map[string]cpsContext  `yaml:"contexts"`
}

type cpsContext struct {
	URL                     string `yaml:"url"`
	Token                   string `yaml:"token"`
	CertificateAuthority    string `yaml:"certificate-authority,omitempty"`
	InsecureSkipTLSVerify   bool   `yaml:"insecure-skip-tls-verify,omitempty"`
}

func defaultConfigPath() string {
	if v := strings.TrimSpace(os.Getenv("CPS_CONFIG")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "cps-config.yaml"
	}
	return filepath.Join(home, ".config", "cps", "config.yaml")
}

func loadConfig(path string) (*cpsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &cpsConfig{Contexts: map[string]cpsContext{}}, nil
		}
		return nil, err
	}
	var cfg cpsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]cpsContext{}
	}
	return &cfg, nil
}

func saveConfig(path string, cfg *cpsConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func resolveActiveContext(cfg *cpsConfig, flagContext string) (name string, ctx cpsContext, err error) {
	name = strings.TrimSpace(flagContext)
	if name == "" {
		name = strings.TrimSpace(cfg.CurrentContext)
	}
	if name == "" {
		return "", cpsContext{}, nil
	}
	ctx, ok := cfg.Contexts[name]
	if !ok {
		return "", cpsContext{}, fmt.Errorf("context %q not found", name)
	}
	return name, ctx, nil
}
