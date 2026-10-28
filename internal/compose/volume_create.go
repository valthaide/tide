package compose

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/syncerr"
)

// externalVolumeAction decides what to do with one external volume name.
// keep means the volume already exists. missing and rejected stop the sync.
// create means the caller should run docker volume create.
func externalVolumeAction(name string, exists, create bool, pattern string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "rejected", nil
	}
	if exists {
		return "keep", nil
	}
	if !create {
		return "missing", nil
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "missing", nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("volume_name_pattern: %w", err)
	}
	if !re.MatchString(name) {
		return "rejected", nil
	}
	return "create", nil
}

func (e *ComposeExecutor) ensureExternalVolume(ctx context.Context, syncLog *strings.Builder, name string, opts ApplyOptions) error {
	action, err := externalVolumeAction(name, e.volumeExists(ctx, name), opts.CreateVolumes, opts.VolumeNamePattern)
	if err != nil {
		appendLogLine(syncLog, err.Error())
		return syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, err.Error())
	}
	switch action {
	case "keep":
		return nil
	case "missing":
		msg := "volume_missing: " + name
		appendLogLine(syncLog, msg)
		return syncerr.New(syncerr.CodeVolumeMissing, syncerr.StageValidate, msg)
	case "rejected":
		msg := "volume_name_rejected: " + name
		appendLogLine(syncLog, msg)
		return syncerr.New(syncerr.CodeVolumeNameRejected, syncerr.StageValidate, msg)
	case "create":
		if err := e.createExternalVolume(ctx, name); err != nil {
			msg := "volume_missing: " + name
			appendLogLine(syncLog, msg)
			return syncerr.New(syncerr.CodeVolumeMissing, syncerr.StageValidate, msg+": "+err.Error())
		}
		appendLogLine(syncLog, "volume_created: "+name)
		if e.Logger != nil {
			e.Logger.Info("Created external volume", "volume", name, "label", "com.conops.managed=true")
		}
		metrics.ObserveVolumeCreated(name)
		if opts.OnVolumeCreated != nil {
			opts.OnVolumeCreated(name)
		}
		return nil
	default:
		msg := "volume_missing: " + name
		appendLogLine(syncLog, msg)
		return syncerr.New(syncerr.CodeVolumeMissing, syncerr.StageValidate, msg)
	}
}

func (e *ComposeExecutor) createExternalVolume(ctx context.Context, name string) error {
	_, err := e.runCommand(ctx, "docker", []string{"volume", "create", "--label", "com.conops.managed=true", name}, e.runtimeWorkDir(), nil, nil)
	return err
}
