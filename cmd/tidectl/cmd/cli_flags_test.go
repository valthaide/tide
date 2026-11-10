package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valthaide/tide/internal/envcompat"
)

func TestTidectlHelpVersionAndUnknownFlags(t *testing.T) {
	envcompat.ResetForTest()
	t.Cleanup(envcompat.ResetForTest)

	var out, errBuf bytes.Buffer
	Version = "v2.0.0"

	if code := ExecuteArgs([]string{"tidectl", "--help"}, &out, &errBuf); code != 0 {
		t.Fatalf("--help exit=%d err=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "tidectl") {
		t.Fatalf("unexpected help output: %s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := ExecuteArgs([]string{"tidectl", "--version"}, &out, &errBuf); code != 0 {
		t.Fatalf("--version exit=%d err=%s", code, errBuf.String())
	}
	if strings.TrimSpace(out.String()) != "tidectl v2.0.0" {
		t.Fatalf("unexpected --version output: %q", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := ExecuteArgs([]string{"tidectl", "--unknown-flag-xyz"}, &out, &errBuf); code == 0 {
		t.Fatal("expected non-zero exit code for unknown flag")
	}
}
