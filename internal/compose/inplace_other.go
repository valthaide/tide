//go:build !linux

package compose

import (
	"context"
	"strings"
)

// materializeInPlace falls back to a normal checkout where file bind mounts
// are not a concern.
func (e *ComposeExecutor) materializeInPlace(ctx context.Context, repoLog *strings.Builder, repoDir, rev string, gitEnv map[string]string) error {
	if _, err := e.runCommandWithTranscript(ctx, repoLog, "git", []string{"checkout", "--force", rev}, repoDir, gitEnv, nil); err != nil {
		return err
	}
	_, err := e.runCommandWithTranscript(ctx, repoLog, "git", []string{"reset", "--hard", rev}, repoDir, gitEnv, nil)
	return err
}
