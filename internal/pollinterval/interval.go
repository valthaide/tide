package pollinterval

import (
	"math/rand/v2"
	"os"
	"strings"
	"time"
)

const (
	defaultApp    = 30 * time.Second
	defaultJitter = 15 * time.Second
)

// AppDefault is the poll interval used when an app does not set poll_interval.
// CONOPS_POLL_INTERVAL overrides it. The built-in default is under 60s.
func AppDefault() time.Duration {
	if d, ok := parseEnv("CONOPS_POLL_INTERVAL"); ok {
		return d
	}
	return defaultApp
}

// Jitter is the extra random delay added to each poll.
// CONOPS_POLL_JITTER overrides it. Zero disables the spread.
func Jitter() time.Duration {
	v := strings.TrimSpace(os.Getenv("CONOPS_POLL_JITTER"))
	if v == "" {
		return defaultJitter
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return defaultJitter
	}
	return d
}

// WithJitter returns base plus a random duration in [0, Jitter].
func WithJitter(base time.Duration) time.Duration {
	j := Jitter()
	if base < 0 {
		base = 0
	}
	if j <= 0 {
		return base
	}
	return base + time.Duration(rand.Int64N(int64(j)+1))
}

func parseEnv(key string) (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
