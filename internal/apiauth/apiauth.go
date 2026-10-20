package apiauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	APITokenEnv       = "CONOPS_API_TOKEN"
	BootstrapFileName = "bootstrap_token"
	HashFileName      = "api_token.sha256"
	TokenPrefix       = "cps_"
)

// Authenticator validates Bearer tokens for the API.
type Authenticator struct {
	hash []byte // sha256 of expected token
}

// Setup loads or creates the API token for dataDir.
// Priority: CONOPS_API_TOKEN env → existing bootstrap/hash → generate new bootstrap.
func Setup(dataDir string) (*Authenticator, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}

	if raw := strings.TrimSpace(os.Getenv(APITokenEnv)); raw != "" {
		h := hashToken(raw)
		if err := os.WriteFile(filepath.Join(dataDir, HashFileName), []byte(hex.EncodeToString(h)), 0600); err != nil {
			return nil, fmt.Errorf("write token hash: %w", err)
		}
		return &Authenticator{hash: h}, nil
	}

	bootstrapPath := filepath.Join(dataDir, BootstrapFileName)
	hashPath := filepath.Join(dataDir, HashFileName)

	if data, err := os.ReadFile(bootstrapPath); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			h := hashToken(token)
			_ = os.WriteFile(hashPath, []byte(hex.EncodeToString(h)), 0600)
			return &Authenticator{hash: h}, nil
		}
	}

	if data, err := os.ReadFile(hashPath); err == nil {
		decoded, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err == nil && len(decoded) == sha256.Size {
			return &Authenticator{hash: decoded}, nil
		}
	}

	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	h := hashToken(token)
	if err := os.WriteFile(bootstrapPath, []byte(token+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("write bootstrap token: %w", err)
	}
	if err := os.WriteFile(hashPath, []byte(hex.EncodeToString(h)), 0600); err != nil {
		return nil, fmt.Errorf("write token hash: %w", err)
	}
	return &Authenticator{hash: h}, nil
}

func (a *Authenticator) Valid(token string) bool {
	if a == nil || len(a.hash) == 0 {
		return false
	}
	got := hashToken(strings.TrimSpace(token))
	return subtle.ConstantTimeCompare(a.hash, got) == 1
}

// Middleware enforces Authorization: Bearer <token> on all requests.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		token := ""
		if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			token = strings.TrimSpace(authz[7:])
		}
		if token == "" || !a.Valid(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="conops"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return TokenPrefix + hex.EncodeToString(buf), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	out := make([]byte, sha256.Size)
	copy(out, sum[:])
	return out
}
