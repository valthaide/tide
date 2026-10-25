package fleetcheck

import "strings"

// PolicyHit is one compose setting the host policy may reject.
type PolicyHit struct {
	Service string
	Rule    string
}

// PolicyException names a service rule that warn mode may skip.
type PolicyException struct {
	Service string
	Rule    string
}

// SecurityHits reports privileged, host networking, and docker.sock mounts.
func SecurityHits(sum Summary) []PolicyHit {
	var hits []PolicyHit
	for _, svc := range sum.Security {
		if svc.Privileged {
			hits = append(hits, PolicyHit{Service: svc.Name, Rule: "privileged"})
		}
		if strings.EqualFold(svc.NetworkMode, "host") {
			hits = append(hits, PolicyHit{Service: svc.Name, Rule: "network_host"})
		}
		if strings.EqualFold(svc.Pid, "host") {
			hits = append(hits, PolicyHit{Service: svc.Name, Rule: "pid_host"})
		}
		if strings.EqualFold(svc.Ipc, "host") {
			hits = append(hits, PolicyHit{Service: svc.Name, Rule: "ipc_host"})
		}
		if len(svc.CapAdd) > 0 {
			hits = append(hits, PolicyHit{Service: svc.Name, Rule: "cap_add"})
		}
		if len(svc.Devices) > 0 {
			hits = append(hits, PolicyHit{Service: svc.Name, Rule: "devices"})
		}
		for _, opt := range svc.SecurityOpt {
			if strings.Contains(strings.ToLower(opt), "unconfined") {
				hits = append(hits, PolicyHit{Service: svc.Name, Rule: "security_unconfined"})
				break
			}
		}
	}
	for _, src := range sum.BindSources {
		clean := strings.TrimSpace(src)
		if clean == "/var/run/docker.sock" || strings.HasSuffix(clean, "/docker.sock") {
			hits = append(hits, PolicyHit{Service: bindService(sum, clean), Rule: "docker_sock"})
		}
		if clean == "/" {
			hits = append(hits, PolicyHit{Service: bindService(sum, clean), Rule: "host_root"})
		}
	}
	return hits
}

func bindService(sum Summary, source string) string {
	for _, svc := range sum.Security {
		for _, src := range svc.Binds {
			if src == source {
				return svc.Name
			}
		}
	}
	return "unknown"
}

// Excepted reports whether warn mode may skip this hit.
func Excepted(hit PolicyHit, allowed []PolicyException) bool {
	for _, item := range allowed {
		if item.Service == hit.Service && item.Rule == hit.Rule && strings.TrimSpace(item.Service) != "" {
			return true
		}
	}
	return false
}
