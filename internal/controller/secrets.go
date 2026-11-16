package controller

import (
	"context"
	"strings"

	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/secrets"
)

func expandSecretPath(path, prefix, tenant, app string) string {
	path = strings.TrimSpace(path)
	repl := strings.NewReplacer(
		"{prefix}", prefix,
		"{tenant}", tenant,
		"{app}", app,
	)
	return strings.Trim(repl.Replace(path), "/")
}

func (r *Reconciler) secretsSpecFor(app *App) (secrets.AppSpec, bool) {
	if r == nil || r.Secrets == nil || app == nil {
		return secrets.AppSpec{}, false
	}
	policy := app.Policy()
	if !r.Secrets.ActiveFor(policy.SecretsProvider) {
		return secrets.AppSpec{}, false
	}
	prefix := r.Secrets.Config().Prefix
	spec := secrets.AppSpec{
		Name:   app.Name,
		Tenant: policy.Tenant,
		File:   policy.EffectiveSecretFile(app.Name),
		From:   map[string]deploypolicy.SecretFrom{},
	}
	for _, path := range policy.SecretPaths {
		spec.Paths = append(spec.Paths, expandSecretPath(path, prefix, policy.Tenant, app.Name))
	}
	spec.Keys = append(spec.Keys, policy.SecretKeys...)
	for key, from := range policy.SecretFrom {
		spec.From[key] = deploypolicy.SecretFrom{
			Path: expandSecretPath(from.Path, prefix, policy.Tenant, app.Name),
			Key:  from.Key,
		}
	}
	return spec, true
}

func (r *Reconciler) renderAppSecrets(ctx context.Context, app *App) error {
	spec, ok := r.secretsSpecFor(app)
	if !ok {
		return nil
	}
	return r.Secrets.RenderApp(ctx, spec)
}

// SecretsSpecs lists provider specs for all apps with an active openbao
// provider; used by the provider poll loop.
func (r *Reconciler) SecretsSpecs() []secrets.AppSpec {
	if r == nil || r.Secrets == nil || r.Registry == nil {
		return nil
	}
	var out []secrets.AppSpec
	for _, app := range r.Registry.List() {
		if app == nil || app.Suspended {
			continue
		}
		if spec, ok := r.secretsSpecFor(app); ok {
			out = append(out, spec)
		}
	}
	return out
}
