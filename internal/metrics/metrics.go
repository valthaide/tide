package metrics

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valthaide/conops-private/internal/giterr"
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
	Branch             string
	Revision           string
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

	DeployBlockedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "deploy_blocked",
		Help:      "1 while a deploy is blocked for this app and reason (ci_pending, ci_failed, ci_error); 0 after the block clears. pipeline_source is the GitLab pipeline that decided the outcome.",
	}, []string{"app", "reason", "pipeline_source"})

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

	AppReloadsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "app_reloads_total",
		Help:      "watch_files reload or restart attempts by result (success or error).",
	}, []string{"app", "result"})

	GitFetchErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "git_fetch_errors_total",
		Help:      "Git fetch or ls-remote failures by repository and reason (auth, network, not_found, other).",
	}, []string{"repo", "reason"})

	GitLastSuccessTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "git_last_success_timestamp_seconds",
		Help:      "Unix timestamp of the last successful git ls-remote or fetch for a repository.",
	}, []string{"repo"})

	LastSyncTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "last_sync_timestamp_seconds",
		Help:      "Unix timestamp of the last successful git check or apply for an app.",
	}, []string{"app"})
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

	appImageInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_image_info",
		Help:      "Running compose image and digest (value always 1).",
	}, []string{"app", "service", "image", "digest"})

	policyViolation = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_policy_violation",
		Help:      "Compose policy violation (1=present).",
	}, []string{"app", "rule"})

	imageDrift = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_image_drift",
		Help:      "Running digest differs from the pinned digest (1=drift).",
	}, []string{"app", "service"})

	fleetRevisionInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "fleet_revision_info",
		Help:      "Fleet checkout revision (value always 1).",
	}, []string{"sha"})

	appVarsInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_vars_info",
		Help:      "Public fleet variables (value always 1). Secrets are never exported.",
	}, []string{"app", "name", "value"})

	SyncQueueLength = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "sync_queue_length",
		Help:      "Rows in the apply queue, including apps not yet pending.",
	})

	SyncRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "sync_running",
		Help:      "Apps currently syncing.",
	})

	PullRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "pull_running",
		Help:      "Image pulls currently running.",
	})

	DeploysTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "deploys_total",
		Help:      "Deploy results recorded for an app.",
	}, []string{"app", "result"})

	ImageGCRemoved = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "image_gc_removed_total",
		Help:      "Images removed by ConOps image GC.",
	})

	ReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reconcile_total",
		Help:      "Reconcile results. noop is a cheap pass with nothing to apply.",
	}, []string{"app", "result", "trigger"})

	QueueWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "sync_queue_wait_seconds",
		Help:      "Seconds an app waited in the apply queue.",
		Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 600, 900},
	}, []string{"trigger"})

	QueueOldest = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "sync_queue_oldest_wait_seconds",
		Help:      "Age of the oldest apply-queue row.",
	})

	Verifying = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "verifying",
		Help:      "Apps whose health check continues after the sync slot was released.",
	})

	appLastReconcile = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_last_reconcile_timestamp",
		Help:      "Unix time of the last reconcile for an app.",
	}, []string{"app"})

	appNextReconcile = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_next_reconcile_timestamp",
		Help:      "Unix time of the next periodic reconcile for an app.",
	}, []string{"app"})

	appReconcileOverdue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_reconcile_overdue",
		Help:      "1 when the last reconcile is older than twice the interval.",
	}, []string{"app"})

	appReconcileAttempt = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "app_reconcile_attempt",
		Help:      "Current retry attempt for an app.",
	}, []string{"app"})
)

// ImageSample is one running service image reported by the reconciler.
type ImageSample struct {
	App     string
	Service string
	Image   string
	Digest  string
}

var imageMu sync.Mutex

// SetAppImages replaces conops_app_image_info with the images that are running now.
func SetAppImages(samples []ImageSample) {
	imageMu.Lock()
	defer imageMu.Unlock()
	appImageInfo.Reset()
	for _, sample := range samples {
		app := strings.TrimSpace(sample.App)
		service := strings.TrimSpace(sample.Service)
		image := strings.TrimSpace(sample.Image)
		digest := strings.TrimSpace(sample.Digest)
		if app == "" || service == "" || image == "" {
			continue
		}
		if digest == "" {
			digest = "unknown"
		}
		appImageInfo.WithLabelValues(app, service, image, digest).Set(1)
	}
}

func SetPolicyViolation(app, rule string, value float64) {
	app = strings.TrimSpace(app)
	rule = strings.TrimSpace(rule)
	if app == "" || rule == "" {
		return
	}
	policyViolation.WithLabelValues(app, rule).Set(value)
}

func SetImageDrift(samples []ImageSample) {
	imageMu.Lock()
	defer imageMu.Unlock()
	imageDrift.Reset()
	for _, sample := range samples {
		if sample.App == "" || sample.Service == "" {
			continue
		}
		imageDrift.WithLabelValues(sample.App, sample.Service).Set(1)
	}
}

var fleetMu sync.Mutex
var fleetSHA string

func SetFleetRevision(sha string) {
	sha = strings.TrimSpace(sha)
	fleetMu.Lock()
	fleetSHA = sha
	fleetMu.Unlock()
	fleetRevisionInfo.Reset()
	if sha != "" {
		fleetRevisionInfo.WithLabelValues(sha).Set(1)
	}
}

func FleetSHA() string {
	fleetMu.Lock()
	defer fleetMu.Unlock()
	return fleetSHA
}

func SetPublicVars(samples []VarSample) {
	imageMu.Lock()
	defer imageMu.Unlock()
	appVarsInfo.Reset()
	for _, sample := range samples {
		if sample.App == "" || sample.Name == "" {
			continue
		}
		appVarsInfo.WithLabelValues(sample.App, sample.Name, sample.Value).Set(1)
	}
}

func SetSyncActivity(queue, running int) {
	SyncQueueLength.Set(float64(queue))
	SyncRunning.Set(float64(running))
}

// ObserveReconcile counts one reconcile result.
func ObserveReconcile(app, result, trigger string) {
	if app == "" {
		app = "unknown"
	}
	if result == "" {
		result = "failed"
	}
	if trigger == "" {
		trigger = "commit"
	}
	ReconcileTotal.WithLabelValues(app, result, trigger).Inc()
}

// ObserveQueueWait records how long an app sat in the queue.
func ObserveQueueWait(trigger string, wait time.Duration) {
	if trigger == "" {
		trigger = "commit"
	}
	if wait < 0 {
		wait = 0
	}
	QueueWait.WithLabelValues(trigger).Observe(wait.Seconds())
}

// SetQueueOldest sets the age of the oldest queued app.
func SetQueueOldest(wait time.Duration) {
	if wait < 0 {
		wait = 0
	}
	QueueOldest.Set(wait.Seconds())
}

// SetVerifying sets how many apps are in background health checks.
func SetVerifying(n int) {
	Verifying.Set(float64(n))
}

// SetReconcileSchedule publishes the last and next reconcile times.
func SetReconcileSchedule(app string, last, next time.Time, overdue bool, attempt int) {
	if app == "" {
		return
	}
	if !last.IsZero() {
		appLastReconcile.WithLabelValues(app).Set(float64(last.Unix()))
	}
	if !next.IsZero() {
		appNextReconcile.WithLabelValues(app).Set(float64(next.Unix()))
	}
	if overdue {
		appReconcileOverdue.WithLabelValues(app).Set(1)
	} else {
		appReconcileOverdue.WithLabelValues(app).Set(0)
	}
	appReconcileAttempt.WithLabelValues(app).Set(float64(attempt))
}

// VarSample is one public variable exported to metrics.
type VarSample struct {
	App   string
	Name  string
	Value string
}

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
		AppReloadsTotal,
		GitFetchErrorsTotal,
		GitLastSuccessTimestamp,
		LastSyncTimestamp,
		envDrift,
		envHashApplied,
		syncErrorInfo,
		appImageInfo,
		policyViolation,
		imageDrift,
		fleetRevisionInfo,
		appVarsInfo,
		SyncQueueLength,
		SyncRunning,
		PullRunning,
		DeploysTotal,
		ImageGCRemoved,
		ReconcileTotal,
		QueueWait,
		QueueOldest,
		Verifying,
		appLastReconcile,
		appNextReconcile,
		appReconcileOverdue,
		appReconcileAttempt,
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
	revisionDesc                    *prometheus.Desc
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
			[]string{"app", "failed_revision", "running_revision", "reason"},
			nil,
		),
		revisionDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "app_revision"),
			"Requested revision and the commit currently deployed (value always 1).",
			[]string{"app", "ref", "sha"},
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
	ch <- c.revisionDesc
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

		ref := strings.TrimSpace(app.Revision)
		if ref == "" {
			ref = strings.TrimSpace(app.Branch)
		}
		if ref == "" {
			ref = "main"
		}
		sha := strings.TrimSpace(app.LastSyncedCommit)
		if sha == "" {
			sha = strings.TrimSpace(app.LastSeenCommit)
		}
		ch <- prometheus.MustNewConstMetric(c.revisionDesc, prometheus.GaugeValue, 1, appLabel, ref, sha)

		rejected := strings.TrimSpace(app.RejectedCommit)
		healthy := strings.TrimSpace(app.LastHealthyCommit)
		seen := strings.TrimSpace(app.LastSeenCommit)
		if rejected != "" && healthy != "" && seen == rejected && strings.TrimSpace(app.LastSyncedCommit) == healthy {
			ch <- prometheus.MustNewConstMetric(c.rolledBackDesc, prometheus.GaugeValue, 1, appLabel, rejected, healthy, rollbackReasonLabel(app.LastSyncError))
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

var deployBlockReasons = []string{"ci_pending", "ci_failed", "ci_error"}

// ObserveDeployBlocked marks deploy_blocked{app,reason,pipeline_source}=1 and clears other series for the app.
func ObserveDeployBlocked(app, reason, source string) {
	app = strings.TrimSpace(app)
	reason = strings.TrimSpace(reason)
	source = strings.TrimSpace(source)
	if app == "" {
		app = "unknown"
	}
	if reason == "" {
		reason = "unknown"
	}
	DeployBlockedTotal.DeletePartialMatch(prometheus.Labels{"app": app})
	known := false
	for _, item := range deployBlockReasons {
		v := 0.0
		if item == reason {
			v = 1
			known = true
		}
		DeployBlockedTotal.WithLabelValues(app, item, source).Set(v)
	}
	if !known {
		DeployBlockedTotal.WithLabelValues(app, reason, source).Set(1)
	}
	CICheckLastTimestamp.WithLabelValues(app).Set(float64(time.Now().Unix()))
}

// ClearDeployBlocked removes deploy_blocked series for the app.
func ClearDeployBlocked(app string) {
	app = strings.TrimSpace(app)
	if app == "" {
		app = "unknown"
	}
	DeployBlockedTotal.DeletePartialMatch(prometheus.Labels{"app": app})
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

func rollbackReasonLabel(msg string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(msg)), "unhealthy") {
		return "unhealthy"
	}
	return "health_timeout"
}

func metricErrorText(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 180 {
		msg = msg[:180]
	}
	return msg
}

// EnsureGitFetchErrors creates a 0 series for every reason so alerts can use
// increase() before the first real failure.
func EnsureGitFetchErrors(repos ...string) {
	reasons := []string{giterr.ReasonAuth, giterr.ReasonNetwork, giterr.ReasonNotFound, giterr.ReasonOther}
	for _, repo := range repos {
		repo = repoLabel(repo)
		if repo == "unknown" {
			continue
		}
		for _, reason := range reasons {
			GitFetchErrorsTotal.WithLabelValues(repo, reason).Add(0)
		}
	}
}

// EnsureAppReloads creates zero series for success and error before the first reload.
func EnsureAppReloads(apps ...string) {
	for _, app := range apps {
		app = strings.TrimSpace(app)
		if app == "" {
			continue
		}
		AppReloadsTotal.WithLabelValues(app, "success").Add(0)
		AppReloadsTotal.WithLabelValues(app, "error").Add(0)
	}
}

// ForgetApp drops metric series for an app removed from fleet.
// repoStillUsed keeps git series when another app still uses the same repository.
func ForgetApp(name, id, repo string, repoStillUsed bool) {
	name = strings.TrimSpace(name)
	id = strings.TrimSpace(id)
	if name != "" {
		for _, labels := range []prometheus.Labels{
			{"app": name},
			{"app_name": name},
		} {
			envDrift.DeletePartialMatch(labels)
			WatchHashErrors.DeletePartialMatch(labels)
			LastSyncTimestamp.DeletePartialMatch(labels)
			AppReloadsTotal.DeletePartialMatch(labels)
			DeployBlockedTotal.DeletePartialMatch(labels)
			CICheckLastTimestamp.DeletePartialMatch(labels)
			WatchActionsTotal.DeletePartialMatch(labels)
			syncErrorInfo.DeletePartialMatch(labels)
			DirtyWorktreeTotal.DeletePartialMatch(labels)
			SyncTotal.DeletePartialMatch(labels)
			SyncDurationSeconds.DeletePartialMatch(labels)
			RequeueTotal.DeletePartialMatch(labels)
			SyncErrorsTotal.DeletePartialMatch(labels)
			envHashApplied.DeletePartialMatch(labels)
			GitAuthFailuresTotal.DeletePartialMatch(prometheus.Labels{"name": name})
		}
	}
	if id != "" {
		for _, labels := range []prometheus.Labels{
			{"app_id": id},
		} {
			SyncTotal.DeletePartialMatch(labels)
			SyncDurationSeconds.DeletePartialMatch(labels)
			RequeueTotal.DeletePartialMatch(labels)
			SyncErrorsTotal.DeletePartialMatch(labels)
			envHashApplied.DeletePartialMatch(labels)
		}
	}
	if repoStillUsed {
		return
	}
	repo = repoLabel(repo)
	if repo == "unknown" {
		return
	}
	GitFetchErrorsTotal.DeletePartialMatch(prometheus.Labels{"repo": repo})
	GitLastSuccessTimestamp.DeletePartialMatch(prometheus.Labels{"repo": repo})
}

// ObserveAppReload counts a watch_files reload or restart.
func ObserveAppReload(app, result string) {
	app = strings.TrimSpace(app)
	if app == "" {
		app = "unknown"
	}
	if result != "success" {
		result = "error"
	}
	AppReloadsTotal.WithLabelValues(app, result).Inc()
}

// ObserveGitFetch records a git ls-remote or fetch. A nil error is a success.
// On failure the caller keeps the last deployed revision; this only counts the error.
func ObserveGitFetch(repo string, err error) {
	repo = repoLabel(repo)
	EnsureGitFetchErrors(repo)
	if err == nil {
		GitLastSuccessTimestamp.WithLabelValues(repo).Set(float64(time.Now().Unix()))
		return
	}
	reason := giterr.Reason(err)
	if reason == "" {
		reason = giterr.ReasonOther
	}
	GitFetchErrorsTotal.WithLabelValues(repo, reason).Inc()
	if reason == giterr.ReasonAuth {
		GitAuthFailuresTotal.WithLabelValues("repo", repo).Inc()
	}
}

// SetLastSyncTimestamp sets conops_last_sync_timestamp_seconds{app}.
func SetLastSyncTimestamp(app string) {
	app = strings.TrimSpace(app)
	if app == "" {
		app = "unknown"
	}
	LastSyncTimestamp.WithLabelValues(app).Set(float64(time.Now().Unix()))
}

func repoLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			raw = raw[:i+3] + rest[at+1:]
		}
	}
	if raw == "" {
		return "unknown"
	}
	return raw
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
