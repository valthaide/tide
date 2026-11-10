package secrets

import (
	"testing"
)

func TestWarningsForSOPS(t *testing.T) {
	t.Setenv("TIDE_SOPS", "true")
	t.Setenv("TIDE_SOPS_AGE_KEY", "")
	t.Setenv("TIDE_SOPS_AGE_KEY_FILE", "")
	cfg := LoadConfigFromEnv()
	warns := cfg.Warnings()
	if len(warns) == 0 {
		t.Fatal("expected warning when TIDE_SOPS=true")
	}
}

func TestNoWarningsWhenClean(t *testing.T) {
	t.Setenv("TIDE_SOPS", "")
	t.Setenv("TIDE_SOPS_AGE_KEY", "")
	t.Setenv("TIDE_SOPS_AGE_KEY_FILE", "")
	cfg := LoadConfigFromEnv()
	if warns := cfg.Warnings(); len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
}
