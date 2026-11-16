package secrets

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/envcompat"
)

const (
	AuthCert    = "cert"
	AuthApprole = "approle"
	AuthJWT     = "jwt"
	AuthToken   = "token"
)

const (
	defaultMount        = "kv"
	defaultPollInterval = time.Minute
	defaultCacheKeyFile = "/etc/tide/cache.key"
	defaultProviderFile = "/etc/tide/secrets-provider.hcl"
)

type Config struct {
	Provider      string
	Addr          string
	CACert        string
	TLSServerName string
	Auth          string
	ClientCert    string
	ClientKey     string
	CertRole      string
	RoleIDFile    string
	SecretIDFile  string
	JWTFile       string
	JWTRole       string
	Token         string
	Mount         string
	Prefix        string
	PollInterval  time.Duration
	Dir           string
	CacheKeyFile  string
}

func LoadConfig() (Config, []string, error) {
	var warnings []string
	cfg := Config{
		Mount:        defaultMount,
		PollInterval: defaultPollInterval,
		Dir:          deploypolicy.DefaultSecretsDir(),
		CacheKeyFile: defaultCacheKeyFile,
	}
	filePath := strings.TrimSpace(envcompat.Getenv("TIDE_SECRETS_PROVIDER_FILE"))
	if filePath == "" {
		filePath = defaultProviderFile
	}
	if data, err := os.ReadFile(filePath); err == nil {
		applyHCLFile(&cfg, string(data))
	} else if explicit := envcompat.Getenv("TIDE_SECRETS_PROVIDER_FILE"); explicit != "" && !os.IsNotExist(err) {
		return cfg, nil, fmt.Errorf("read secrets provider file %s: %w", filePath, err)
	}

	cfg.Provider = deploypolicy.NormalizeSecretsProvider(firstNonEmpty(env("TIDE_SECRETS_PROVIDER"), cfg.Provider))
	cfg.Addr = firstNonEmpty(env("TIDE_OPENBAO_ADDR"), cfg.Addr)
	cfg.CACert = firstNonEmpty(env("TIDE_OPENBAO_CACERT"), cfg.CACert)
	cfg.TLSServerName = firstNonEmpty(env("TIDE_OPENBAO_TLS_SERVER_NAME"), cfg.TLSServerName)
	cfg.Auth = strings.ToLower(firstNonEmpty(env("TIDE_OPENBAO_AUTH"), cfg.Auth))
	cfg.ClientCert = firstNonEmpty(env("TIDE_OPENBAO_CLIENT_CERT"), cfg.ClientCert)
	cfg.ClientKey = firstNonEmpty(env("TIDE_OPENBAO_CLIENT_KEY"), cfg.ClientKey)
	cfg.CertRole = firstNonEmpty(env("TIDE_OPENBAO_CERT_ROLE"), cfg.CertRole)
	cfg.RoleIDFile = firstNonEmpty(env("TIDE_OPENBAO_ROLE_ID_FILE"), cfg.RoleIDFile)
	cfg.SecretIDFile = firstNonEmpty(env("TIDE_OPENBAO_SECRET_ID_FILE"), cfg.SecretIDFile)
	cfg.JWTFile = firstNonEmpty(env("TIDE_OPENBAO_JWT_FILE"), cfg.JWTFile)
	cfg.JWTRole = firstNonEmpty(env("TIDE_OPENBAO_JWT_ROLE"), cfg.JWTRole)
	cfg.Token = firstNonEmpty(env("TIDE_OPENBAO_TOKEN"), cfg.Token)
	cfg.Mount = firstNonEmpty(env("TIDE_OPENBAO_MOUNT"), cfg.Mount)
	cfg.Prefix = firstNonEmpty(env("TIDE_OPENBAO_PREFIX"), cfg.Prefix)
	cfg.CacheKeyFile = firstNonEmpty(env("TIDE_SECRETS_CACHE_KEY_FILE"), cfg.CacheKeyFile)
	if v := env("TIDE_SECRETS_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Second {
			return cfg, nil, fmt.Errorf("invalid TIDE_SECRETS_POLL_INTERVAL: %s", v)
		}
		cfg.PollInterval = d
	}
	if v := env("TIDE_SECRETS_DIR"); v != "" {
		cfg.Dir = v
	}

	if raw := envcompat.Getenv("TIDE_SECRETS_PROVIDER"); strings.EqualFold(strings.TrimSpace(raw), "vault") {
		warnings = append(warnings, "TIDE_SECRETS_PROVIDER=vault is deprecated; use openbao")
	}
	if addr := env("TIDE_VAULT_ADDR"); addr != "" {
		warnings = append(warnings, "TIDE_VAULT_ADDR is deprecated; mapped to TIDE_OPENBAO_ADDR")
		if cfg.Addr == "" {
			cfg.Addr = addr
		}
	}
	if token := env("TIDE_VAULT_TOKEN"); token != "" {
		warnings = append(warnings, "TIDE_VAULT_TOKEN is deprecated; mapped to TIDE_OPENBAO_AUTH=token")
		if cfg.Token == "" {
			cfg.Token = token
		}
		if cfg.Auth == "" {
			cfg.Auth = AuthToken
		}
	}
	if env("TIDE_VAULT_NAMESPACE") != "" {
		warnings = append(warnings, "TIDE_VAULT_NAMESPACE is not supported by the OpenBao provider and is ignored")
	}
	if envcompat.Getenv("TIDE_SOPS") != "" || envcompat.Getenv("TIDE_SOPS_AGE_KEY") != "" || envcompat.Getenv("TIDE_SOPS_AGE_KEY_FILE") != "" {
		warnings = append(warnings, "TIDE_SOPS* is set but SOPS support was removed; unset it")
	}

	cfg.Addr = strings.TrimRight(cfg.Addr, "/")
	if cfg.Provider == "" {
		cfg.Provider = deploypolicy.SecretsProviderNone
	}
	if cfg.Provider != deploypolicy.SecretsProviderNone &&
		cfg.Provider != deploypolicy.SecretsProviderAgent &&
		cfg.Provider != deploypolicy.SecretsProviderOpenBao {
		return cfg, nil, fmt.Errorf("invalid TIDE_SECRETS_PROVIDER: %s (want none, agent, or openbao)", cfg.Provider)
	}
	if cfg.OpenBaoConfigured() {
		if cfg.Auth == "" {
			cfg.Auth = AuthCert
		}
		switch cfg.Auth {
		case AuthCert, AuthApprole, AuthJWT, AuthToken:
		default:
			return cfg, nil, fmt.Errorf("invalid TIDE_OPENBAO_AUTH: %s (want cert, approle, jwt, or token)", cfg.Auth)
		}
		if cfg.Auth == AuthToken {
			warnings = append(warnings, "TIDE_OPENBAO_AUTH=token is intended for tests only; use cert, approle, or jwt in production")
		}
	}
	return cfg, warnings, nil
}

func (c Config) OpenBaoConfigured() bool {
	return c.Addr != ""
}

func env(key string) string {
	return strings.TrimSpace(envcompat.Getenv(key))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func applyHCLFile(cfg *Config, text string) {
	setters := map[string]func(string){
		"provider":        func(v string) { cfg.Provider = v },
		"addr":            func(v string) { cfg.Addr = v },
		"cacert":          func(v string) { cfg.CACert = v },
		"tls_server_name": func(v string) { cfg.TLSServerName = v },
		"auth":            func(v string) { cfg.Auth = v },
		"client_cert":     func(v string) { cfg.ClientCert = v },
		"client_key":      func(v string) { cfg.ClientKey = v },
		"cert_role":       func(v string) { cfg.CertRole = v },
		"role_id_file":    func(v string) { cfg.RoleIDFile = v },
		"secret_id_file":  func(v string) { cfg.SecretIDFile = v },
		"jwt_file":        func(v string) { cfg.JWTFile = v },
		"jwt_role":        func(v string) { cfg.JWTRole = v },
		"token":           func(v string) { cfg.Token = v },
		"mount":           func(v string) { cfg.Mount = v },
		"prefix":          func(v string) { cfg.Prefix = v },
		"dir":             func(v string) { cfg.Dir = v },
		"cache_key_file":  func(v string) { cfg.CacheKeyFile = v },
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if set, ok := setters[key]; ok {
			set(value)
		}
	}
}
