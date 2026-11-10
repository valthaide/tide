package apiauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/valthaide/tide/internal/envcompat"
)

const (
	APITokenEnv       = "TIDE_API_TOKEN"
	OperatorTokensEnv = "TIDE_OPERATOR_TOKENS"
	ForceTokensEnv    = "TIDE_FORCE_TOKENS"
	BootstrapFileName = "bootstrap_token"
	HashFileName      = "api_token.sha256"
	TokenPrefix       = "tide_"
)

type contextKey string

const (
	actorContextKey    contextKey = "tide_actor"
	canForceContextKey contextKey = "tide_can_force"
)

var validActorRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

type namedToken struct {
	actor string
	hash  []byte
}

// Authenticator validates Bearer tokens for the API and resolves operator identities for audit logs.
type Authenticator struct {
	hash             []byte // sha256 of primary admin token
	operators        []namedToken
	forceOperators   []namedToken
	restrictForceSet bool
}

// Setup loads or creates the API token for dataDir.
// Priority: TIDE_API_TOKEN env → existing bootstrap/hash → generate new bootstrap.
func Setup(dataDir string) (*Authenticator, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}

	auth := &Authenticator{
		operators: parseNamedTokens(envcompat.Getenv(OperatorTokensEnv)),
	}
	if rawForce := envcompat.Getenv(ForceTokensEnv); rawForce != "" {
		auth.restrictForceSet = true
		auth.forceOperators = parseNamedTokens(rawForce)
	}

	if raw := envcompat.Getenv(APITokenEnv); raw != "" {
		h := hashToken(raw)
		if err := os.WriteFile(filepath.Join(dataDir, HashFileName), []byte(hex.EncodeToString(h)), 0600); err != nil {
			return nil, fmt.Errorf("write token hash: %w", err)
		}
		auth.hash = h
		return auth, nil
	}

	bootstrapPath := filepath.Join(dataDir, BootstrapFileName)
	hashPath := filepath.Join(dataDir, HashFileName)

	if data, err := os.ReadFile(bootstrapPath); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			h := hashToken(token)
			_ = os.WriteFile(hashPath, []byte(hex.EncodeToString(h)), 0600)
			auth.hash = h
			return auth, nil
		}
	}

	if data, err := os.ReadFile(hashPath); err == nil {
		decoded, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err == nil && len(decoded) == sha256.Size {
			auth.hash = decoded
			return auth, nil
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
	auth.hash = h
	return auth, nil
}

func parseNamedTokens(raw string) []namedToken {
	var out []namedToken
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		actor, tok, ok := strings.Cut(entry, ":")
		actor = strings.TrimSpace(actor)
		tok = strings.TrimSpace(tok)
		if !ok || actor == "" || tok == "" {
			continue
		}
		out = append(out, namedToken{actor: actor, hash: hashToken(tok)})
	}
	return out
}

// Authenticate checks a token and returns (valid, actor, canForce).
func (a *Authenticator) Authenticate(token, headerActor string) (bool, string, bool) {
	if a == nil {
		return false, "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return false, "", false
	}
	got := hashToken(token)

	for _, fop := range a.forceOperators {
		if subtle.ConstantTimeCompare(fop.hash, got) == 1 {
			return true, fop.actor, true
		}
	}
	for _, op := range a.operators {
		if subtle.ConstantTimeCompare(op.hash, got) == 1 {
			return true, op.actor, !a.restrictForceSet
		}
	}
	if len(a.hash) > 0 && subtle.ConstantTimeCompare(a.hash, got) == 1 {
		actor := sanitizeActor(headerActor)
		if actor == "" {
			actor = "admin"
		}
		return true, actor, !a.restrictForceSet
	}
	return false, "", false
}

func (a *Authenticator) Valid(token string) bool {
	ok, _, _ := a.Authenticate(token, "")
	return ok
}

func sanitizeActor(raw string) string {
	raw = strings.TrimSpace(raw)
	if validActorRe.MatchString(raw) {
		return raw
	}
	return ""
}

// ActorFromRequest returns the authenticated actor for audit records.
func ActorFromRequest(r *http.Request) string {
	if r == nil {
		return "system"
	}
	if v, ok := r.Context().Value(actorContextKey).(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	if hdr := sanitizeActor(r.Header.Get("X-Tide-Actor")); hdr != "" {
		return hdr
	}
	return "admin"
}

// CanForceBypass reports whether the caller is authorized to bypass the CI gate.
// In unit tests that call handlers directly without Middleware, true is returned unless
// Middleware explicitly recorded false.
func CanForceBypass(r *http.Request) bool {
	if r == nil {
		return false
	}
	if v, ok := r.Context().Value(canForceContextKey).(bool); ok {
		return v
	}
	return true
}

// Middleware enforces Authorization: Bearer <token> on all requests.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		token := ""
		if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			token = strings.TrimSpace(authz[7:])
		}
		ok, actor, canForce := a.Authenticate(token, r.Header.Get("X-Tide-Actor"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tide"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), actorContextKey, actor)
		ctx = context.WithValue(ctx, canForceContextKey, canForce)
		next.ServeHTTP(w, r.WithContext(ctx))
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
