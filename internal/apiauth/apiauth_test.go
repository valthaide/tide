package apiauth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupGeneratesBootstrapToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(APITokenEnv, "")

	auth, err := Setup(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, BootstrapFileName))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(data))
	if !auth.Valid(token) {
		t.Fatal("bootstrap token should validate")
	}

	called := false
	h := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || !called {
		t.Fatalf("expected auth success, got %d", rr.Code)
	}
}
