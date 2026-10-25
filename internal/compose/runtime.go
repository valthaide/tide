package compose

import (
	"encoding/json"
	"fmt"
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

type containerInspect struct {
	ID           string `json:"Id"`
	Image        string `json:"Image"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		ExitCode  int    `json:"ExitCode"`
		StartedAt string `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

type imageInspect struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	RepoTags    []string `json:"RepoTags"`
}

// applyContainerInspectJSON merges `docker inspect` JSON into observations.
// Health may be absent. RepoDigests are not read here; they belong to the image.
func applyContainerInspectJSON(obs []containerObs, raw []byte) error {
	payload := extractJSON(string(raw))
	if len(payload) == 0 {
		return fmt.Errorf("docker inspect returned no json")
	}
	var docs []containerInspect
	if err := json.Unmarshal(payload, &docs); err != nil {
		return err
	}
	byID := map[string]int{}
	for i := range obs {
		byID[containerKey(obs[i].ID)] = i
	}
	for _, doc := range docs {
		idx, ok := byID[containerKey(doc.ID)]
		if !ok {
			continue
		}
		obs[idx].HasInspect = true
		obs[idx].State = strings.TrimSpace(doc.State.Status)
		obs[idx].ExitCode = doc.State.ExitCode
		obs[idx].RestartCount = doc.RestartCount
		obs[idx].RestartPolicy = strings.TrimSpace(doc.HostConfig.RestartPolicy.Name)
		obs[idx].ImageID = strings.TrimSpace(doc.Image)
		if doc.Config.Labels != nil {
			obs[idx].DependsOn = strings.TrimSpace(doc.Config.Labels["com.docker.compose.depends_on"])
		}
		if doc.State.Health != nil {
			obs[idx].Health = strings.TrimSpace(doc.State.Health.Status)
		}
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(doc.State.StartedAt)); err == nil {
			obs[idx].StartedAt = t
		}
		obs[idx].Running = doc.State.Running || obs[idx].State == "running" || dockerStatusIsRunning(obs[idx].PSStatus)
	}
	return nil
}

// applyImageInspectJSON copies RepoDigests from `docker image inspect` onto containers.
func applyImageInspectJSON(obs []containerObs, raw []byte) error {
	payload := extractJSON(string(raw))
	if len(payload) == 0 {
		return fmt.Errorf("docker image inspect returned no json")
	}
	var docs []imageInspect
	if err := json.Unmarshal(payload, &docs); err != nil {
		return err
	}
	byID := map[string]string{}
	byRef := map[string]string{}
	for _, doc := range docs {
		if len(doc.RepoDigests) == 0 {
			continue
		}
		digest := doc.RepoDigests[0]
		byID[containerKey(doc.ID)] = digest
		for _, tag := range doc.RepoTags {
			tag = strings.TrimSpace(tag)
			if tag != "" {
				byRef[tag] = digest
			}
		}
	}
	for i := range obs {
		if d := byID[containerKey(obs[i].ImageID)]; d != "" {
			obs[i].RepoDigest = d
			continue
		}
		if d := byRef[strings.TrimSpace(obs[i].Image)]; d != "" {
			obs[i].RepoDigest = d
		}
	}
	return nil
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
			if state.Failure == "" {
				service := c.Service
				if service == "" {
					service = name
				}
				state.Failure = fmt.Sprintf("oneshot_failed: %s exit %d", service, c.ExitCode)
			}
		case isOneshot && (c.Running || c.State == "created" || c.State == "restarting"):
			state.ContainerCount++
			state.StartingCount++
			state.NotReady = append(state.NotReady, name)
		case flapping(c, now):
			state.ContainerCount++
			state.FlappingCount++
			state.NotReady = append(state.NotReady, name)
			service := c.Service
			if service == "" {
				service = name
			}
			state.Failure = fmt.Sprintf("restarting: %s %d restarts", service, c.RestartCount)
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
