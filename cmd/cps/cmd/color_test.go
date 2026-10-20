package cmd

import "testing"

func TestColorEnabledAlways(t *testing.T) {
	t.Setenv("CPS_COLOR", "always")
	t.Setenv("NO_COLOR", "1")
	if !colorEnabled() {
		t.Fatal("CPS_COLOR=always should override NO_COLOR")
	}
	t.Setenv("CPS_COLOR", "")
	t.Setenv("NO_COLOR", "1")
	if colorEnabled() {
		t.Fatal("NO_COLOR should disable when CPS_COLOR unset")
	}
	t.Setenv("CPS_COLOR", "never")
	t.Setenv("NO_COLOR", "")
	if colorEnabled() {
		t.Fatal("CPS_COLOR=never should disable")
	}
}
