package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// cpsConfig is the on-disk kubeconfig-like file for cps.
type cpsConfig struct {
	CurrentContext string                `yaml:"current-context"`
	Contexts       map[string]cpsContext `yaml:"contexts"`
}

type cpsContext struct {
	URL                   string `yaml:"url"`
	Token                 string `yaml:"token"`
	CertificateAuthority  string `yaml:"certificate-authority,omitempty"`
	InsecureSkipTLSVerify bool   `yaml:"insecure-skip-tls-verify,omitempty"`
}

func defaultConfigPath() string {
	if v := strings.TrimSpace(os.Getenv("CPS_CONFIG")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "cps-config.yaml"
	}
	// Preferred: ~/.conops/config
	preferred := filepath.Join(home, ".conops", "config")
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	// Legacy fallback if only the old path exists.
	legacy := filepath.Join(home, ".config", "cps", "config.yaml")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return preferred
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

// parseEnvFile reads KEY=VALUE lines (shell-style comments and blanks ignored).
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key != "" {
			out[key] = val
		}
	}
	return out, sc.Err()
}
