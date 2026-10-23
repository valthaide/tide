package metrics

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
)

// MetricsToken reads CONOPS_METRICS_TOKEN, or the trimmed contents of
// CONOPS_METRICS_TOKEN_FILE when the env value is empty.
// An empty result means the metrics endpoint stays open.
func MetricsToken() string {
	if v := strings.TrimSpace(os.Getenv("CONOPS_METRICS_TOKEN")); v != "" {
		return v
	}
	path := strings.TrimSpace(os.Getenv("CONOPS_METRICS_TOKEN_FILE"))
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Protect returns handler, or a bearer check in front of it when token is set.
func Protect(token string, handler http.Handler) http.Handler {
	token = strings.TrimSpace(token)
	if token == "" || handler == nil {
		return handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bearerOK(r.Header.Get("Authorization"), token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func bearerOK(header, token string) bool {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(header[len(prefix):])
	if len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
