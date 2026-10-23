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
	Suspended          bool
	LastSyncAt         time.Time
	LastSyncError      string
	LastSyncErrorCode  string
	LastSyncErrorStage string
	LastSeenCommit     string
	LastSyncedCommit   string
	LastHealthyCommit  string
	RejectedCommit     string
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

	GitAuthFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "git_auth_failures_total",
		Help:      "Number of git authentication failures (app or fleet).",
	}, []string{"scope", "name"})

	FleetLastSuccessTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "fleet_last_success_timestamp_seconds",
		Help:      "Unix timestamp of the last successful fleet reconcile.",
	})

	FleetErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "fleet_errors_total",
		Help:      "Number of fleet reconcile failures by code.",
	}, []string{"code"})

	ReconcileLastSuccessTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "reconcile_last_success_timestamp",
		Help:      "Unix timestamp of the last successful app reconcile/apply.",
	})

	DeployBlockedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "deploy_blocked",
		Help:      "Number of deploys blocked before apply (ci_pending|ci_failed|ci_error).",
	}, []string{"app", "reason"})

	CICheckLastTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "ci_check_last_timestamp_seconds",
		Help:      "Unix timestamp of the last CI pipeline gate check per app.",
	}, []string{"app"})

	WatchActionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "watch_file_actions_total",
		Help:      "Number of watch_files restart/reload actions applied.",
	}, []string{"app"})

	WatchHashErrors = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "watch_files_hash_error",
		Help:      "1 when the last watch_files hash for an app failed.",
	}, []string{"app"})

	DirtyWorktreeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "dirty_worktree_total",
		Help:      "Number of sync attempts that found a dirty git worktree.",
	}, []string{"app", "action"})

	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Build version information (value always 1).",
	}, []string{"version"})

	FleetHostFileMissing = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "fleet_host_file_missing",
		Help:      "1 while hosts/<CONOPS_FLEET_HOST>.yaml is absent from the fleet git checkout.",
	})
)

var (
	envDrift = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "env_drift",
		Help:      "Whether external compose env_file content differs from last applied hash (1=yes).",
	}, []string{"app"})

	envHashApplied = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_env_hash_applied_timestamp_seconds",
		Help:      "Unix timestamp when the current external env_file hash was last applied.",
	}, []string{"app_id", "app_name"})

	syncErrorInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_sync_error_info",
		Help:      "Last sync error code/stage as a gauge (1=active for that code/stage).",
	}, []string{"app", "code", "stage"})
)

// Register registers process counters/histograms and an apps collector.
func Register(registry prometheus.Registerer, lister AppLister) error {
	collectors := []prometheus.Collector{
		ReconcileLoopsTotal,
		SyncTotal,
		SyncDurationSeconds,
		RequeueTotal,
		SyncErrorsTotal,
		GitAuthFailuresTotal,
		FleetLastSuccessTimestamp,
		FleetErrorsTotal,
		ReconcileLastSuccessTimestamp,
		DeployBlockedTotal,
		CICheckLastTimestamp,
		WatchActionsTotal,
		WatchHashErrors,
		DirtyWorktreeTotal,
		BuildInfo,
		FleetHostFileMissing,
		envDrift,
		envHashApplied,
		syncErrorInfo,
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

	appsDesc                        *prometheus.Desc
	syncStatusDesc                  *prometheus.Desc
	lastSyncTimestampDesc           *prometheus.Desc
	lastSyncErrorDesc               *prometheus.Desc
	commitDriftDesc                 *prometheus.Desc
	syncInProgressDesc              *prometheus.Desc
	lastSuccessfulSyncTimestampDesc *prometheus.Desc
	syncSuccessAgeSecondsDesc       *prometheus.Desc
	suspendedDesc                   *prometheus.Desc
	rolledBackDesc                  *prometheus.Desc
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
			[]string{"app_id", "app_name", "code", "stage", "error"},
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
		suspendedDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_suspended"),
			"Whether automatic reconciliation is paused for the app (1=yes, 0=no).",
			[]string{"app"},
			nil,
		),
		rolledBackDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_rolled_back"),
			"1 while a failed revision was rolled back and the last healthy revision is running.",
			[]string{"app", "failed_revision", "running_revision"},
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
	ch <- c.suspendedDesc
	ch <- c.rolledBackDesc
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
		errText := ""
		if strings.TrimSpace(app.LastSyncError) != "" {
			hasError = 1
			errText = metricErrorText(app.LastSyncError)
		}
		ch <- prometheus.MustNewConstMetric(c.lastSyncErrorDesc, prometheus.GaugeValue, hasError, id, name, code, stage, errText)

		var suspended float64
		if app.Suspended {
			suspended = 1
		}
		appLabel := name
		if appLabel == "" {
			appLabel = id
		}
		ch <- prometheus.MustNewConstMetric(c.suspendedDesc, prometheus.GaugeValue, suspended, appLabel)

		rejected := strings.TrimSpace(app.RejectedCommit)
		healthy := strings.TrimSpace(app.LastHealthyCommit)
		seen := strings.TrimSpace(app.LastSeenCommit)
		if rejected != "" && healthy != "" && seen == rejected && strings.TrimSpace(app.LastSyncedCommit) == healthy {
			ch <- prometheus.MustNewConstMetric(c.rolledBackDesc, prometheus.GaugeValue, 1, appLabel, rejected, healthy)
		}

		var drift float64
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
		SetSyncErrorInfo(appName, code, stage)
		if code == "git_auth" {
			GitAuthFailuresTotal.WithLabelValues("app", appName).Inc()
		}
	} else {
		ClearSyncErrorInfo(appName)
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

// SetEnvDrift sets whether an app's external env_file hash differs from applied.
func SetEnvDrift(app string, drifted bool) {
	app = strings.TrimSpace(app)
	if app == "" {
		app = "unknown"
	}
	v := 0.0
	if drifted {
		v = 1
	}
	envDrift.WithLabelValues(app).Set(v)
}

// ObserveReconcileSuccess records last successful reconcile timestamp.
func ObserveReconcileSuccess() {
	ReconcileLastSuccessTimestamp.Set(float64(time.Now().Unix()))
}

// ObserveDeployBlocked increments deploy_blocked for an app/reason.
func ObserveDeployBlocked(app, reason string) {
	app = strings.TrimSpace(app)
	reason = strings.TrimSpace(reason)
	if app == "" {
		app = "unknown"
	}
	if reason == "" {
		reason = "unknown"
	}
	DeployBlockedTotal.WithLabelValues(app, reason).Inc()
	CICheckLastTimestamp.WithLabelValues(app).Set(float64(time.Now().Unix()))
}

// SetWatchHashError sets whether the last watch_files hash failed.
func SetWatchHashError(app string, failed bool) {
	app = strings.TrimSpace(app)
	if app == "" {
		app = "unknown"
	}
	v := 0.0
	if failed {
		v = 1
	}
	WatchHashErrors.WithLabelValues(app).Set(v)
}

// ObserveDirtyWorktree increments dirty worktree counter.
func ObserveDirtyWorktree(app, action string) {
	app = strings.TrimSpace(app)
	action = strings.TrimSpace(action)
	if app == "" {
		app = "unknown"
	}
	if action == "" {
		action = "refuse"
	}
	DirtyWorktreeTotal.WithLabelValues(app, action).Inc()
}

// ObserveWatchAction increments watch_file action counter.
func ObserveWatchAction(app string) {
	app = strings.TrimSpace(app)
	if app == "" {
		app = "unknown"
	}
	WatchActionsTotal.WithLabelValues(app).Inc()
}

func metricErrorText(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 180 {
		msg = msg[:180]
	}
	return msg
}

// SetFleetHostFileMissing sets conops_fleet_host_file_missing.
func SetFleetHostFileMissing(missing bool) {
	if missing {
		FleetHostFileMissing.Set(1)
		return
	}
	FleetHostFileMissing.Set(0)
}

// SetBuildInfo sets conops_build_info{version=...} 1.
func SetBuildInfo(version string) {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "dev"
	}
	BuildInfo.Reset()
	BuildInfo.WithLabelValues(version).Set(1)
}

// SetEnvHashApplied records when an env hash was applied.
func SetEnvHashApplied(appID, appName string, at time.Time) {
	if at.IsZero() {
		return
	}
	envHashApplied.WithLabelValues(appID, appName).Set(float64(at.Unix()))
}

// SetSyncErrorInfo exposes the latest sync error code/stage for alerts.
func SetSyncErrorInfo(app, code, stage string) {
	app = strings.TrimSpace(app)
	if app == "" {
		return
	}
	syncErrorInfo.DeletePartialMatch(prometheus.Labels{"app": app})
	if code == "" {
		code = "unknown"
	}
	if stage == "" {
		stage = "unknown"
	}
	syncErrorInfo.WithLabelValues(app, code, stage).Set(1)
}

// ClearSyncErrorInfo clears the last sync error info gauge for an app.
func ClearSyncErrorInfo(app string) {
	app = strings.TrimSpace(app)
	if app == "" {
		return
	}
	syncErrorInfo.DeletePartialMatch(prometheus.Labels{"app": app})
}

// ObserveFleetSuccess records a successful fleet reconcile.
func ObserveFleetSuccess() {
	FleetLastSuccessTimestamp.Set(float64(time.Now().Unix()))
}

// ObserveFleetError increments fleet error counter.
func ObserveFleetError(code string) {
	code = strings.TrimSpace(code)
	if code == "" {
		code = "unknown"
	}
	FleetErrorsTotal.WithLabelValues(code).Inc()
	if code == "git_auth" {
		GitAuthFailuresTotal.WithLabelValues("fleet", "fleet").Inc()
	}
}
