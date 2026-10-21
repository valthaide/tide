package secrets

import (
	"testing"
)

func TestValidateRejectsSOPS(t *testing.T) {
	t.Setenv("CONOPS_SOPS", "true")
	t.Setenv("CONOPS_SOPS_AGE_KEY", "")
	t.Setenv("CONOPS_SOPS_AGE_KEY_FILE", "")
	cfg := LoadConfigFromEnv()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when CONOPS_SOPS=true")
	}
}

func TestValidateOKWhenSOPSOff(t *testing.T) {
	t.Setenv("CONOPS_SOPS", "false")
	t.Setenv("CONOPS_SOPS_AGE_KEY", "")
	t.Setenv("CONOPS_SOPS_AGE_KEY_FILE", "")
	cfg := LoadConfigFromEnv()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
