package giterr

import (
	"errors"
	"testing"
)

func TestReason(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"authentication required", ReasonAuth},
		{"HTTP Basic: Access denied", ReasonAuth},
		{"revision \"v9\" not found", ReasonNotFound},
		{"dial tcp: i/o timeout", ReasonNetwork},
		{"connection refused", ReasonNetwork},
		{"object not in pack", ReasonOther},
	}
	for _, tc := range cases {
		if got := Reason(errors.New(tc.msg)); got != tc.want {
			t.Fatalf("%q: got %s want %s", tc.msg, got, tc.want)
		}
	}
	if Reason(nil) != "" {
		t.Fatal("nil")
	}
}
