package compose

import (
	"fmt"
	"os"
	"strings"
)

// RegistryCreds is the registry login puppet passes into the process environment.
type RegistryCreds struct {
	Host     string
	Username string
	Password string
}

// RegistryCredsFromEnv reads REGISTRY_HOST, REGISTRY_USERNAME and REGISTRY_PASSWORD.
// CONOPS_REGISTRY_* is accepted as an alias. All empty means login is not configured.
// A partial set is an error.
func RegistryCredsFromEnv() (RegistryCreds, bool, error) {
	host := firstEnv("REGISTRY_HOST", "CONOPS_REGISTRY_HOST")
	user := firstEnv("REGISTRY_USERNAME", "CONOPS_REGISTRY_USERNAME")
	pass := firstEnv("REGISTRY_PASSWORD", "CONOPS_REGISTRY_PASSWORD")
	if host == "" && user == "" && pass == "" {
		return RegistryCreds{}, false, nil
	}
	if host == "" || user == "" || pass == "" {
		return RegistryCreds{}, true, fmt.Errorf("registry_auth")
	}
	return RegistryCreds{Host: host, Username: user, Password: pass}, true, nil
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}
