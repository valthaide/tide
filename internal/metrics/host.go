package metrics

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	"github.com/valthaide/tide/internal/envcompat"
)

var (
	hostOnce sync.Once
	hostName string
)

// Host returns the uniform host name used in metric labels: TIDE_FLEET_HOST
// when set, otherwise the machine hostname expanded to its FQDN.
func Host() string {
	hostOnce.Do(func() {
		hostName = resolveHost()
	})
	return hostName
}

func resolveHost() string {
	if h := strings.TrimSpace(envcompat.Getenv("TIDE_FLEET_HOST")); h != "" {
		return h
	}
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	h = strings.TrimSpace(h)
	if h == "" {
		return "unknown"
	}
	if strings.Contains(h, ".") {
		return h
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if cname, err := net.DefaultResolver.LookupCNAME(ctx, h); err == nil {
		cname = strings.TrimSuffix(strings.TrimSpace(cname), ".")
		if strings.Contains(cname, ".") {
			return cname
		}
	}
	return h
}

type hostLabelGatherer struct {
	gatherer prometheus.Gatherer
}

func (g hostLabelGatherer) Gather() ([]*dto.MetricFamily, error) {
	families, err := g.gatherer.Gather()
	host := Host()
	for _, mf := range families {
		if mf == nil {
			continue
		}
		for _, m := range mf.GetMetric() {
			if m == nil || hasHostLabel(m) {
				continue
			}
			m.Label = append(m.Label, &dto.LabelPair{
				Name:  proto.String("host"),
				Value: proto.String(host),
			})
		}
	}
	return families, err
}

func hasHostLabel(m *dto.Metric) bool {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == "host" {
			return true
		}
	}
	return false
}
