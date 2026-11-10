package cmd

import "testing"

func TestColorEnabledAlways(t *testing.T) {
	t.Setenv("TIDECTL_COLOR", "always")
	t.Setenv("NO_COLOR", "1")
	if !colorEnabled() {
		t.Fatal("TIDECTL_COLOR=always should override NO_COLOR")
	}
	t.Setenv("TIDECTL_COLOR", "")
	t.Setenv("NO_COLOR", "1")
	if colorEnabled() {
		t.Fatal("NO_COLOR should disable when TIDECTL_COLOR unset")
	}
	t.Setenv("TIDECTL_COLOR", "never")
	t.Setenv("NO_COLOR", "")
	if colorEnabled() {
		t.Fatal("TIDECTL_COLOR=never should disable")
	}
}
