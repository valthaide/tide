package metrics

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProtectMetricsToken(t *testing.T) {
	body := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	open := Protect("", body)
	rec := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	open.ServeHTTP(rr, rec)
	if rr.Code != http.StatusOK {
		t.Fatalf("open: %d", rr.Code)
	}

	locked := Protect("secret", body)
	rr = httptest.NewRecorder()
	locked.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rr.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr = httptest.NewRecorder()
	locked.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", rr.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	locked.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Fatalf("good token: %d %q", rr.Code, rr.Body.String())
	}
}

func TestMetricsTokenFile(t *testing.T) {
	t.Setenv("CONOPS_METRICS_TOKEN", "")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONOPS_METRICS_TOKEN_FILE", path)
	if got := MetricsToken(); got != "from-file" {
		t.Fatalf("file token %q", got)
	}
	t.Setenv("CONOPS_METRICS_TOKEN", "from-env")
	if got := MetricsToken(); got != "from-env" {
		t.Fatalf("env token %q", got)
	}
}
