package pollinterval

import (
	"testing"
	"time"
)

func TestAppDefaultUnderOneMinute(t *testing.T) {
	if AppDefault() > time.Minute {
		t.Fatalf("default %s", AppDefault())
	}
	t.Setenv("TIDE_POLL_INTERVAL", "45s")
	if AppDefault() != 45*time.Second {
		t.Fatalf("env %s", AppDefault())
	}
}

func TestWithJitterStaysNearBase(t *testing.T) {
	t.Setenv("TIDE_POLL_JITTER", "10s")
	base := 30 * time.Second
	for i := 0; i < 20; i++ {
		got := WithJitter(base)
		if got < base || got > base+10*time.Second {
			t.Fatalf("jitter %s outside [%s, %s]", got, base, base+10*time.Second)
		}
	}
}
