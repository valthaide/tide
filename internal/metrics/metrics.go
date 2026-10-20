package metrics

import (
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "conops"

// AppSnapshot is the app state exposed to Prometheus gauges.
type AppSnapshot struct {
	ID                 string
	Name               string
	Status             string
	LastSyncAt         time.Time
	LastSyncError      string
	LastSyncErrorCode  string
	LastSyncErrorStage string
	LastSeenCommit     string
	LastSyncedCommit   string
}

// AppLister provides app snapshots for gauge metrics on each scrape.
type AppLister interface {
	ListAppSnapshots() []AppSnapshot
}

var (
	ReconcileLoopsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reconcile_loops_total",
		Help:      "Number of reconciler loop iterations.",
	})

	SyncTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "app_sync_total",
		Help:      "Number of app sync attempts by result.",
	}, []string{"app_id", "app_name", "result"})

	SyncDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "app_sync_duration_seconds",
		Help:      "Duration of app sync apply operations.",
		Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 600},
	}, []string{"app_id", "app_name"})

	RequeueTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "app_requeue_total",
		Help:      "Number of times an app was requeued for reconciliation, by reason.",
	}, []string{"app_id", "app_name", "reason"})

	SyncErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "app_sync_errors_total",
		Help:      "Number of sync failures by structured code and stage.",
	}, []string{"app_id", "app_name", "code", "stage"})
)

// Register registers process counters/histograms and an apps collector.
func Register(registry prometheus.Registerer, lister AppLister) error {
	collectors := []prometheus.Collector{
		ReconcileLoopsTotal,
		SyncTotal,
		SyncDurationSeconds,
		RequeueTotal,
		SyncErrorsTotal,
		newAppsCollector(lister),
	}
	for _, c := range collectors {
		if err := registry.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// Handler returns an HTTP handler for GET /metrics.
func Handler(registry prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

type appsCollector struct {
	lister AppLister

	appsDesc                      *prometheus.Desc
	syncStatusDesc                *prometheus.Desc
	lastSyncTimestampDesc         *prometheus.Desc
	lastSyncErrorDesc             *prometheus.Desc
	commitDriftDesc               *prometheus.Desc
	syncInProgressDesc            *prometheus.Desc
	lastSuccessfulSyncTimestampDesc *prometheus.Desc
	syncSuccessAgeSecondsDesc     *prometheus.Desc
}

func newAppsCollector(lister AppLister) *appsCollector {
	return &appsCollector{
		lister: lister,
		appsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "apps"),
			"Number of registered apps by status.",
			[]string{"status"},
			nil,
		),
		syncStatusDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_sync_status"),
			"Current sync status of an app (1 for the active status).",
			[]string{"app_id", "app_name", "status"},
			nil,
		),
		lastSyncTimestampDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_last_sync_timestamp_seconds"),
			"Unix timestamp of the last sync attempt for an app.",
			[]string{"app_id", "app_name"},
			nil,
		),
		lastSyncErrorDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_last_sync_error"),
			"Whether the app currently has a last sync error (1=yes, 0=no).",
			[]string{"app_id", "app_name", "code", "stage"},
			nil,
		),
		commitDriftDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_commit_drift"),
			"Whether last_seen_commit differs from last_synced_commit (1=yes, 0=no).",
			[]string{"app_id", "app_name"},
			nil,
		),
		syncInProgressDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_sync_in_progress"),
			"Whether an app sync is currently in progress (1=yes, 0=no).",
			[]string{"app_id", "app_name"},
			nil,
		),
		lastSuccessfulSyncTimestampDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_last_successful_sync_timestamp_seconds"),
			"Unix timestamp of the last successful sync (status=synced); 0 if never synced successfully.",
			[]string{"app_id", "app_name"},
			nil,
		),
		syncSuccessAgeSecondsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_sync_success_age_seconds"),
			"Seconds since the last successful sync; -1 if never synced successfully.",
			[]string{"app_id", "app_name"},
			nil,
		),
	}
}

func (c *appsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.appsDesc
	ch <- c.syncStatusDesc
	ch <- c.lastSyncTimestampDesc
	ch <- c.lastSyncErrorDesc
	ch <- c.commitDriftDesc
	ch <- c.syncInProgressDesc
	ch <- c.lastSuccessfulSyncTimestampDesc
	ch <- c.syncSuccessAgeSecondsDesc
}

func (c *appsCollector) Collect(ch chan<- prometheus.Metric) {
	if c.lister == nil {
		return
	}

	now := time.Now()
	apps := c.lister.ListAppSnapshots()
	statusCounts := map[string]float64{}

	for _, app := range apps {
		id := strings.TrimSpace(app.ID)
		name := strings.TrimSpace(app.Name)
		status := strings.TrimSpace(app.Status)
		if status == "" {
			status = "unknown"
		}
		statusCounts[status]++

		ch <- prometheus.MustNewConstMetric(c.syncStatusDesc, prometheus.GaugeValue, 1, id, name, status)

		var lastSync float64
		if !app.LastSyncAt.IsZero() {
			lastSync = float64(app.LastSyncAt.Unix())
		}
		ch <- prometheus.MustNewConstMetric(c.lastSyncTimestampDesc, prometheus.GaugeValue, lastSync, id, name)

		var hasError float64
		code := strings.TrimSpace(app.LastSyncErrorCode)
		stage := strings.TrimSpace(app.LastSyncErrorStage)
		if code == "" {
			code = "none"
		}
		if stage == "" {
			stage = "none"
		}
		if strings.TrimSpace(app.LastSyncError) != "" {
			hasError = 1
		}
		ch <- prometheus.MustNewConstMetric(c.lastSyncErrorDesc, prometheus.GaugeValue, hasError, id, name, code, stage)

		var drift float64
		seen := strings.TrimSpace(app.LastSeenCommit)
		synced := strings.TrimSpace(app.LastSyncedCommit)
		if seen != "" && seen != synced {
			drift = 1
		}
		ch <- prometheus.MustNewConstMetric(c.commitDriftDesc, prometheus.GaugeValue, drift, id, name)

		var inProgress float64
		if status == "syncing" {
			inProgress = 1
		}
		ch <- prometheus.MustNewConstMetric(c.syncInProgressDesc, prometheus.GaugeValue, inProgress, id, name)

		var lastSuccess float64
		age := float64(-1)
		if status == "synced" && !app.LastSyncAt.IsZero() && strings.TrimSpace(app.LastSyncError) == "" {
			lastSuccess = float64(app.LastSyncAt.Unix())
			age = now.Sub(app.LastSyncAt).Seconds()
			if age < 0 {
				age = 0
			}
		}
		ch <- prometheus.MustNewConstMetric(c.lastSuccessfulSyncTimestampDesc, prometheus.GaugeValue, lastSuccess, id, name)
		ch <- prometheus.MustNewConstMetric(c.syncSuccessAgeSecondsDesc, prometheus.GaugeValue, age, id, name)
	}

	for status, count := range statusCounts {
		ch <- prometheus.MustNewConstMetric(c.appsDesc, prometheus.GaugeValue, count, status)
	}
}

// ObserveSync records sync attempt metrics.
func ObserveSync(appID, appName string, duration time.Duration, err error) {
	result := "success"
	if err != nil {
		result = "error"
		code, stage := "unknown", "unknown"
		type coded interface {
			GetCode() string
			GetStage() string
		}
		if c, ok := err.(coded); ok {
			code, stage = c.GetCode(), c.GetStage()
		}
		SyncErrorsTotal.WithLabelValues(appID, appName, code, stage).Inc()
	}
	SyncTotal.WithLabelValues(appID, appName, result).Inc()
	SyncDurationSeconds.WithLabelValues(appID, appName).Observe(duration.Seconds())
}

// ObserveRequeue records a reconciliation requeue with a reason label.
func ObserveRequeue(appID, appName, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "unknown"
	}
	RequeueTotal.WithLabelValues(appID, appName, reason).Inc()
}
