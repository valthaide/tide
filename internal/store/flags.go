package store

import "github.com/valthaide/tide/internal/api"

// applyRuntimeFlags sets pause and remediation fields loaded from the database.
// Suspended is the effective pause: fleet git or an operator suspend.
func applyRuntimeFlags(app *api.App, fleetSuspended, operatorSuspended bool) {
	if app == nil {
		return
	}
	app.FleetSuspended = fleetSuspended
	app.OperatorSuspended = operatorSuspended
	app.Suspended = fleetSuspended || operatorSuspended
	app.ApplyPolicy()
}
