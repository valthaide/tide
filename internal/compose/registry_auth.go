package compose

import (
	"fmt"

	"github.com/valthaide/tide/internal/envcompat"
)

// RegistryCreds is the registry login passed into the process environment.
type RegistryCreds struct {
	Host     string
	Username string
	Password string
}

// RegistryCredsFromEnv reads REGISTRY_HOST, REGISTRY_USERNAME and REGISTRY_PASSWORD.
// TIDE_REGISTRY_* is accepted as an alias. All empty means login is not configured.
// A partial set is an error.
func RegistryCredsFromEnv() (RegistryCreds, bool, error) {
	host := envcompat.FirstEnv("REGISTRY_HOST", "TIDE_REGISTRY_HOST")
	user := envcompat.FirstEnv("REGISTRY_USERNAME", "TIDE_REGISTRY_USERNAME")
	pass := envcompat.FirstEnv("REGISTRY_PASSWORD", "TIDE_REGISTRY_PASSWORD")
	if host == "" && user == "" && pass == "" {
		return RegistryCreds{}, false, nil
	}
	if host == "" || user == "" || pass == "" {
		return RegistryCreds{}, true, fmt.Errorf("registry_auth")
	}
	return RegistryCreds{Host: host, Username: user, Password: pass}, true, nil
}
