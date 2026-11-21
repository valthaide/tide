package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/deploypolicy"
)

// PlanApp builds the apply plan for one app — which services a sync would
// create, recreate, restart, or remove, which volumes it would create, and
// which watch_files actions would fire — without changing anything.
// fleetRef, when set, renders the fleet template at that not-yet-applied
// fleet revision (for fleet CI); the live fleet checkout is not modified.
func (r *Reconciler) PlanApp(ctx context.Context, app *App, fleetRef string) (*compose.AppPlan, error) {
	if r == nil || r.Executor == nil {
		return nil, fmt.Errorf("runtime executor is not configured")
	}
	if app == nil {
		return nil, fmt.Errorf("app is nil")
	}
	planner, ok := r.Executor.(interface {
		PlanApp(ctx context.Context, appID, composePath string, envVars map[string]string, opts compose.ApplyOptions, in compose.PlanInput) (*compose.AppPlan, error)
	})
	if !ok {
		return nil, fmt.Errorf("runtime does not support planning")
	}
	if !r.checkoutExists(app.ID) {
		return nil, fmt.Errorf("app %s has no repository checkout yet; run a sync first", app.Name)
	}
	if err := r.renderAppSecrets(ctx, app); err != nil {
		return nil, err
	}
	envPaths, err := r.envFilePaths(app)
	if err != nil {
		return nil, err
	}
	fileVars, missing := readEnvFiles(envPaths)
	if missing != "" {
		return nil, fmt.Errorf("env_file_missing: %s", missing)
	}
	envVars, err := r.Registry.GetAppEnvs(app.ID)
	if err != nil {
		return nil, err
	}
	policy := app.Policy()
	opts := compose.ApplyOptions{
		Template: strings.TrimSpace(app.Template),
		AppDir:   strings.TrimSpace(app.AppDir),
		Project:  app.ComposeProject(),
		Vars:     mergeInterpolation(fileVars, policy.Vars, app.Name, app.ComposeProject()),
	}
	if fleetRef = strings.TrimSpace(fleetRef); fleetRef != "" {
		if opts.Template == "" {
			return nil, fmt.Errorf("fleet_ref planning requires a template-based app")
		}
		direr, ok := r.Executor.(interface{ DefaultFleetCheckoutDir() string })
		if !ok || strings.TrimSpace(direr.DefaultFleetCheckoutDir()) == "" {
			return nil, fmt.Errorf("fleet_ref planning requires a fleet checkout on this controller")
		}
		dir, cleanup, err := compose.MaterializeTemplateAtRef(direr.DefaultFleetCheckoutDir(), fleetRef, opts.Template)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		opts.FleetCheckoutDir = dir
	}

	in := compose.PlanInput{}
	if cur, err := r.envHash(app); err == nil && cur.Hash != "" &&
		strings.TrimSpace(app.LastAppliedEnvHash) != "" && cur.Hash != app.LastAppliedEnvHash {
		in.EnvChangedPaths = changedEnvPaths(decodeEnvPathHashes(app.LastAppliedEnvPaths), cur.PerPath)
	}
	in.WatchChanged = r.planWatchChanges(app, policy.WatchFiles)

	return planner.PlanApp(ctx, app.ID, app.ComposePath, envVars, opts, in)
}

// planWatchChanges lists watch_files entries whose content hash drifted from
// the last applied baseline, mapped to the action a reconcile would run.
func (r *Reconciler) planWatchChanges(app *App, files []deploypolicy.WatchFile) []compose.WatchPlanEntry {
	if len(files) == 0 || strings.TrimSpace(app.LastAppliedWatchHash) == "" {
		return nil
	}
	mgr, ok := r.Executor.(WatchFileManager)
	if !ok {
		return nil
	}
	hashes, err := mgr.WatchEntryHashesForApp(app.ID, files)
	if err != nil {
		return nil
	}
	prev, legacy := decodeWatchHashes(app.LastAppliedWatchHash)
	var changed []deploypolicy.WatchFile
	switch {
	case len(prev) > 0:
		for _, w := range files {
			if prev[w.Path] != hashes[w.Path] {
				changed = append(changed, w)
			}
		}
	case legacy != "":
		combined, err := mgr.WatchFilesHashForApp(app.ID, files)
		if err != nil || combined == legacy {
			return nil
		}
		changed = files
	default:
		return nil
	}
	out := make([]compose.WatchPlanEntry, 0, len(changed))
	for _, w := range changed {
		out = append(out, compose.WatchPlanEntry{Path: w.Path, Service: w.Service, Action: watchPlanAction(w)})
	}
	return out
}

// watchPlanAction maps a watch_files entry to the action applyWatchActions
// would run: reload only with a reload command or URL, otherwise a restart.
func watchPlanAction(w deploypolicy.WatchFile) string {
	if w.Action == "reload" && (strings.TrimSpace(w.ReloadCommand) != "" || strings.TrimSpace(w.ReloadURL) != "") {
		return "reload"
	}
	return "restart"
}
