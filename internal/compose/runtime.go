package compose

import (
	"strings"
	"time"
)

// containerObs is one compose container as seen from docker.
type containerObs struct {
	ID            string
	Project       string
	Service       string
	Name          string
	OneOff        bool
	PSStatus      string
	State         string // running, exited, restarting, ...
	ExitCode      int
	RestartCount  int
	StartedAt     time.Time
	RestartPolicy string
	DependsOn     string
	Health        string
	Image         string
	ImageID       string
	RepoDigest    string
	Running       bool
	HasInspect    bool
}

// oneshotServices returns service names that other containers wait to complete.
// Compose records that on the dependent container as
// "migrate:service_completed_successfully:false,...".
func oneshotServices(obs []containerObs) map[string]struct{} {
	out := map[string]struct{}{}
	for _, c := range obs {
		for _, name := range servicesCompletedSuccessfully(c.DependsOn) {
			out[name] = struct{}{}
		}
	}
	return out
}

func servicesCompletedSuccessfully(label string) []string {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil
	}
	var names []string
	for _, part := range strings.Split(label, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, ":")
		if len(fields) < 2 {
			continue
		}
		if strings.TrimSpace(fields[1]) == "service_completed_successfully" {
			name := strings.TrimSpace(fields[0])
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// flapping reports a container that is up only because it keeps restarting.
// A container without a healthcheck is not healthy merely because it is running.
func flapping(c containerObs, now time.Time) bool {
	state := strings.ToLower(strings.TrimSpace(c.State))
	ps := strings.ToLower(c.PSStatus)
	if state == "restarting" || strings.Contains(ps, "restarting") {
		return true
	}
	if !c.Running || c.RestartCount < 3 || c.StartedAt.IsZero() {
		return false
	}
	return now.Sub(c.StartedAt) < 2*time.Minute
}

// foldObservations groups containers into per-project health.
// A one-shot service that exited 0 is finished work, not a failed container.
func foldObservations(obs []containerObs, now time.Time) map[string]ProjectRuntimeState {
	oneshot := oneshotServices(obs)
	out := map[string]ProjectRuntimeState{}
	for _, c := range obs {
		if c.Project == "" || c.OneOff {
			continue
		}
		state := out[c.Project]
		_, isOneshot := oneshot[c.Service]
		name := c.Name
		if name == "" {
			name = c.Service
		}
		switch {
		case isOneshot && !c.Running && c.ExitCode == 0 && (c.State == "exited" || strings.HasPrefix(strings.ToLower(c.PSStatus), "exited (0)")):
			// finished one-shot; do not count it as a container that must stay up
		case isOneshot && !c.Running && c.HasInspect && c.ExitCode != 0:
			state.ContainerCount++
			state.UnhealthyCount++
			state.NotReady = append(state.NotReady, name)
		case isOneshot && (c.Running || c.State == "created" || c.State == "restarting"):
			state.ContainerCount++
			state.StartingCount++
			state.NotReady = append(state.NotReady, name)
		case flapping(c, now):
			state.ContainerCount++
			state.FlappingCount++
			state.NotReady = append(state.NotReady, name)
		default:
			state.ContainerCount++
			running := c.Running || dockerStatusIsRunning(c.PSStatus)
			if running {
				state.RunningCount++
			} else {
				state.ExitedCount++
			}
			lower := strings.ToLower(c.PSStatus + " " + c.Health)
			unhealthy := strings.Contains(lower, "unhealthy") || c.Health == "unhealthy"
			starting := strings.Contains(lower, "starting") || c.Health == "starting"
			if unhealthy {
				state.UnhealthyCount++
			}
			if starting {
				state.StartingCount++
			}
			if name != "" && (unhealthy || starting || !running) {
				state.NotReady = append(state.NotReady, name)
			}
		}
		out[c.Project] = state
	}
	return out
}
