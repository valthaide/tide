package controller

import (
	"context"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/valthaide/conops-private/internal/api"
	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/syncerr"
)

// QueueItem is one app waiting for an apply slot.
type QueueItem struct {
	AppID    string    `json:"app_id"`
	Name     string    `json:"app"`
	Trigger  string    `json:"trigger"`
	Class    int       `json:"class"`
	Priority int       `json:"priority"`
	QueuedAt time.Time `json:"queued_at"`
	Reasons  []string  `json:"reasons,omitempty"`
	Running  bool      `json:"running,omitempty"`
}

func triggerClass(trigger string) int {
	switch trigger {
	case "manual", "rollback":
		return 0
	case "commit", "fleet":
		return 1
	case "drift":
		return 2
	case "retry":
		return 3
	default:
		return 4
	}
}

func triggerForReason(reason string) string {
	switch {
	case strings.Contains(reason, "commit"):
		return "commit"
	case strings.Contains(reason, "var"), strings.Contains(reason, "env"), strings.Contains(reason, "watch"), strings.Contains(reason, "fleet"):
		return "fleet"
	case strings.Contains(reason, "image"), strings.Contains(reason, "runtime"), strings.Contains(reason, "container"), strings.Contains(reason, "restart"):
		return "drift"
	case strings.Contains(reason, "retry"), strings.Contains(reason, "unhealthy"):
		return "retry"
	default:
		return "drift"
	}
}

func (r *Reconciler) appInterval(app *App) (time.Duration, bool) {
	if app != nil {
		raw := strings.TrimSpace(app.Policy().ReconcileInterval)
		if raw == "off" || raw == "0" {
			return 0, true
		}
		if raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d >= 30*time.Second {
				return d, false
			}
		}
	}
	if r != nil && r.Config.AppReconcileOff {
		return 0, true
	}
	d := time.Duration(0)
	if r != nil {
		d = r.Config.AppReconcileInterval
	}
	if d <= 0 {
		d = 5 * time.Minute
	}
	return d, false
}

func (r *Reconciler) jitter(interval time.Duration) time.Duration {
	maxJitter := interval / 5
	if maxJitter > time.Minute {
		maxJitter = time.Minute
	}
	if r != nil && r.Config.ReconcileJitter > 0 {
		maxJitter = r.Config.ReconcileJitter
		if maxJitter > time.Minute {
			maxJitter = time.Minute
		}
	}
	if maxJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(maxJitter) + 1))
}

func (r *Reconciler) rememberNext(id string, next, last time.Time) {
	r.mu.Lock()
	if r.nextAt == nil {
		r.nextAt = map[string]time.Time{}
	}
	if r.lastAt == nil {
		r.lastAt = map[string]time.Time{}
	}
	if !next.IsZero() {
		r.nextAt[id] = next
	}
	if !last.IsZero() {
		r.lastAt[id] = last
	}
	r.mu.Unlock()
	if r.Registry != nil {
		_ = r.Registry.SetNextReconcile(id, next, last)
	}
}

func (r *Reconciler) reconcileDue(app *App, interval time.Duration) bool {
	r.mu.Lock()
	next, ok := r.nextAt[app.ID]
	r.mu.Unlock()
	if !ok && r.Registry != nil {
		loaded, _, _, _, _, err := r.Registry.LoadSchedule(app.ID)
		if err == nil && !loaded.IsZero() {
			next = loaded
			ok = true
			r.mu.Lock()
			if r.nextAt == nil {
				r.nextAt = map[string]time.Time{}
			}
			r.nextAt[app.ID] = next
			r.mu.Unlock()
		}
	}
	if !ok || next.IsZero() {
		half := interval / 2
		if half < time.Second {
			half = time.Second
		}
		spread := half + time.Duration(rand.Int63n(int64(half)+1))
		r.rememberNext(app.ID, time.Now().Add(spread), time.Time{})
		return false
	}
	if time.Now().Before(next) {
		return false
	}
	r.rememberNext(app.ID, time.Now().Add(interval+r.jitter(interval)), time.Now())
	return true
}

func (r *Reconciler) runPeriodic(apps []*App, snap map[string]compose.ProjectRuntimeState) {
	if r == nil {
		return
	}
	for _, app := range apps {
		if app == nil || app.Suspended {
			continue
		}
		interval, off := r.appInterval(app)
		if off {
			metrics.SetReconcileSchedule(app.Name, time.Time{}, time.Time{}, false, 0)
			continue
		}
		if !r.reconcileDue(app, interval) {
			r.publishSchedule(app, interval)
			continue
		}
		r.publishSchedule(app, interval)
		if r.configHold(app) {
			metrics.ObserveReconcile(app.Name, "skipped", "periodic")
			continue
		}
		reason, trigger := r.cheapDrift(app, snap)
		if reason == "" {
			metrics.ObserveReconcile(app.Name, "noop", "periodic")
			continue
		}
		if app.Status == "syncing" || app.Status == "verifying" {
			r.enqueue(app, trigger, reason, appPriority(app))
			if app.Status == "syncing" && r.Registry != nil {
				_ = r.Registry.BumpDesired(app.ID)
			}
		} else {
			r.requeuePending(app, reason)
		}
		if r.Logger != nil {
			r.Logger.Info("Periodic reconcile queued", "app", app.Name, "trigger", trigger, "reason", reason)
		}
	}
}

func (r *Reconciler) publishSchedule(app *App, interval time.Duration) {
	r.mu.Lock()
	last := r.lastAt[app.ID]
	next := r.nextAt[app.ID]
	r.mu.Unlock()
	overdue := !last.IsZero() && time.Since(last) > 2*interval
	attempt, _, _ := r.Registry.RetryState(app.ID)
	metrics.SetReconcileSchedule(app.Name, last, next, overdue, attempt)
}

func (r *Reconciler) cheapDrift(app *App, snap map[string]compose.ProjectRuntimeState) (string, string) {
	if app.LastSeenCommit != "" && app.LastSeenCommit != app.LastSyncedCommit {
		return "commit_drift", "commit"
	}
	if reason := r.varsDriftReason(app); reason != "" {
		return reason, "fleet"
	}
	if reason := r.envFileDriftReason(app); reason != "" {
		return reason, "fleet"
	}
	if snap != nil {
		if reason := r.runtimeDriftReason(app.ComposeProject(), snap); reason != "" {
			return reason, "drift"
		}
	}
	if reason := r.imageDriftReason(app); reason != "" {
		return reason, "drift"
	}
	return "", ""
}

func (r *Reconciler) imageDriftReason(app *App) string {
	if app == nil || r.Executor == nil {
		return ""
	}
	source, ok := r.Executor.(interface {
		ExpectedDigests(project string) map[string]string
	})
	if !ok {
		return ""
	}
	want := source.ExpectedDigests(app.ComposeProject())
	if len(want) == 0 {
		return ""
	}
	r.mu.Lock()
	images := append([]compose.RunningImage(nil), r.running...)
	r.mu.Unlock()
	for _, img := range images {
		if img.Project != app.ComposeProject() {
			continue
		}
		digest := want[img.Service]
		if digest != "" && img.Digest != "" && !strings.Contains(img.Digest, digest) && !strings.Contains(digest, img.Digest) {
			return "image_drift"
		}
	}
	return ""
}

func (r *Reconciler) configHold(app *App) bool {
	if app == nil || (app.Status != "error" && app.Status != "failed") {
		return false
	}
	if !configError(app) {
		return false
	}
	if app.LastSeenCommit != "" && app.LastSeenCommit != app.LastSyncedCommit {
		return false
	}
	if r.varsDriftReason(app) != "" {
		return false
	}
	return true
}

func configError(app *App) bool {
	se := appTransientError(app)
	if se == nil {
		return false
	}
	switch se.Code {
	case syncerr.CodeValidation, syncerr.CodePortConflict, syncerr.CodeDigestRequired, syncerr.CodeRegistryDenied:
		return true
	}
	text := strings.ToLower(se.Message)
	return strings.Contains(text, "policy_violation") || strings.Contains(text, "project_conflict") || strings.Contains(text, "port_conflict") || strings.Contains(text, "digest_required")
}

func appPriority(app *App) int {
	if app == nil {
		return 0
	}
	if p := app.Policy().Priority; p != nil {
		return *p
	}
	return 0
}

func (r *Reconciler) enqueue(app *App, trigger, reason string, priority int) {
	if r == nil || app == nil {
		return
	}
	if trigger == "" {
		trigger = triggerForReason(reason)
	}
	now := time.Now()
	r.mu.Lock()
	if r.queue == nil {
		r.queue = map[string]*QueueItem{}
	}
	item := r.queue[app.ID]
	if item == nil {
		item = &QueueItem{
			AppID:    app.ID,
			Name:     app.Name,
			Trigger:  trigger,
			Class:    triggerClass(trigger),
			Priority: priority,
			QueuedAt: now,
		}
		r.queue[app.ID] = item
	} else {
		if triggerClass(trigger) < item.Class {
			item.Trigger = trigger
			item.Class = triggerClass(trigger)
		}
		if priority > item.Priority {
			item.Priority = priority
		}
	}
	if reason != "" {
		seen := false
		for _, existing := range item.Reasons {
			if existing == reason {
				seen = true
				break
			}
		}
		if !seen {
			item.Reasons = append(item.Reasons, reason)
		}
	}
	class := item.Class
	storedTrigger := item.Trigger
	queuedAt := item.QueuedAt
	r.mu.Unlock()
	if r.Registry != nil {
		_ = r.Registry.SetQueued(app.ID, queuedAt, storedTrigger, class)
	}
	if r.Logger != nil && reason != "" && reason != "pending" {
		r.Logger.Info("Enqueued app", "app", app.Name, "trigger", storedTrigger, "class", class, "reason", reason)
	}
}

func (r *Reconciler) ensureQueued(app *App, trigger, reason string) {
	r.mu.Lock()
	_, ok := r.queue[app.ID]
	r.mu.Unlock()
	if ok {
		return
	}
	r.enqueue(app, trigger, reason, appPriority(app))
}

func (r *Reconciler) markQueueStart(id string) {
	r.mu.Lock()
	item := r.queue[id]
	r.mu.Unlock()
	if item != nil && !item.QueuedAt.IsZero() {
		metrics.ObserveQueueWait(item.Trigger, time.Since(item.QueuedAt))
	}
}

func (r *Reconciler) dequeue(id string) {
	r.mu.Lock()
	delete(r.queue, id)
	r.mu.Unlock()
	if r.Registry != nil {
		_ = r.Registry.ClearQueued(id)
	}
}

func (r *Reconciler) orderByQueue(apps []*App) []*App {
	for _, app := range apps {
		if app != nil && !app.Suspended && app.Status == "pending" {
			r.ensureQueued(app, "commit", "pending")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sort.SliceStable(apps, func(i, j int) bool {
		return queueLess(r.queue[apps[i].ID], r.queue[apps[j].ID], apps[i].Name, apps[j].Name, r.Config.QueueAging)
	})
	return apps
}

func queueLess(a, b *QueueItem, nameA, nameB string, aging time.Duration) bool {
	if a == nil && b == nil {
		return nameA < nameB
	}
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	ca, cb := agedClass(a, aging), agedClass(b, aging)
	if ca != cb {
		return ca < cb
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if !a.QueuedAt.Equal(b.QueuedAt) {
		return a.QueuedAt.Before(b.QueuedAt)
	}
	return nameA < nameB
}

func agedClass(item *QueueItem, aging time.Duration) int {
	if item == nil {
		return 4
	}
	if aging <= 0 {
		aging = 10 * time.Minute
	}
	class := item.Class
	if !item.QueuedAt.IsZero() {
		steps := int(time.Since(item.QueuedAt) / aging)
		class -= steps
		if class < 0 {
			class = 0
		}
	}
	return class
}

// NoteManual puts an app at the front of the apply queue.
func (r *Reconciler) NoteManual(app *api.App) {
	r.enqueue(app, "manual", "manual", appPriority(app))
}

// Snapshot returns the apply queue in run order.
func (r *Reconciler) Snapshot() []QueueItem {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]QueueItem, 0, len(r.queue))
	for id, item := range r.queue {
		copyItem := *item
		if _, ok := r.inFlight[id]; ok {
			copyItem.Running = true
		}
		items = append(items, copyItem)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return queueLess(&items[i], &items[j], items[i].Name, items[j].Name, r.Config.QueueAging)
	})
	return items
}

// Position reports 1-based queue position and class.
func (r *Reconciler) Position(id string) (int, int) {
	items := r.Snapshot()
	for i, item := range items {
		if item.AppID == id {
			return i + 1, item.Class
		}
	}
	return 0, 0
}

// Annotate fills queue fields on the API view.
func (r *Reconciler) Annotate(apps []*api.App) {
	if r == nil {
		return
	}
	items := r.Snapshot()
	pos := map[string]int{}
	byID := map[string]QueueItem{}
	for i, item := range items {
		pos[item.AppID] = i + 1
		byID[item.AppID] = item
	}
	for _, app := range apps {
		if app == nil {
			continue
		}
		r.mu.Lock()
		last := r.lastAt[app.ID]
		next := r.nextAt[app.ID]
		r.mu.Unlock()
		app.LastReconcile = last
		app.NextReconcile = next
		app.Priority = appPriority(app)
		if item, ok := byID[app.ID]; ok {
			app.Trigger = item.Trigger
			app.Queue = strconv.Itoa(pos[app.ID])
			app.QueueClass = item.Class
		}
		if attempt, _, err := r.Registry.RetryState(app.ID); err == nil {
			app.Attempt = attempt
		}
	}
}

func (r *Reconciler) publishQueue() {
	items := r.Snapshot()
	oldest := time.Duration(0)
	warnAfter := 15 * time.Minute
	if r.Config.QueueWarnAfter > 0 {
		warnAfter = r.Config.QueueWarnAfter
	}
	now := time.Now()
	for _, item := range items {
		if item.Running || item.QueuedAt.IsZero() {
			continue
		}
		wait := now.Sub(item.QueuedAt)
		if wait > oldest {
			oldest = wait
		}
		if wait >= warnAfter && r.Logger != nil {
			r.mu.Lock()
			if r.warnedQueue == nil {
				r.warnedQueue = map[string]time.Time{}
			}
			last := r.warnedQueue[item.AppID]
			if last.IsZero() || now.Sub(last) >= warnAfter {
				r.warnedQueue[item.AppID] = now
				r.mu.Unlock()
				r.Logger.Warn("App queued longer than expected", "app", item.Name, "trigger", item.Trigger, "wait", wait.Round(time.Second).String())
			} else {
				r.mu.Unlock()
			}
		}
	}
	metrics.SetQueueOldest(oldest)
	running := 0
	r.mu.Lock()
	running = len(r.inFlight)
	r.mu.Unlock()
	metrics.SetSyncActivity(len(items), running)
}

// SetLimit changes how many apps may sync at once, up to the semaphore size.
func (r *Reconciler) SetLimit(n int) {
	if r == nil || n < 1 {
		return
	}
	r.mu.Lock()
	if cap(r.sem) > 0 && n > cap(r.sem) {
		n = cap(r.sem)
	}
	r.limit = n
	r.mu.Unlock()
}

func (r *Reconciler) SkipCI(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.skipCI == nil {
		r.skipCI = map[string]struct{}{}
	}
	r.skipCI[id] = struct{}{}
}

func (r *Reconciler) consumeSkipCI(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.skipCI[id]; !ok {
		return false
	}
	delete(r.skipCI, id)
	return true
}

func (r *Reconciler) currentTrigger(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if item := r.queue[id]; item != nil && item.Trigger != "" {
		return item.Trigger
	}
	return "commit"
}

// releaseHealthSlot starts a background health check when the slot should not
// be held until healthy. The default until_healthy path does not call this.
func (r *Reconciler) releaseHealthSlot(app *App, target, output string, started time.Time, gen int64) bool {
	if r == nil || !r.Config.SlotHealthRelease || app == nil {
		return false
	}
	hold := r.Config.SlotHealthHold
	if hold > 0 {
		short, cancel := context.WithTimeout(context.Background(), hold)
		err := r.confirmHealthy(short, app, target)
		cancel()
		if err == nil {
			return false
		}
	}
	now := time.Now()
	_ = r.Registry.UpdateStatus(app.ID, "verifying", &now)
	app.Status = "verifying"
	metrics.SetVerifying(1)
	go r.finishVerify(app.ID, target, output, started, gen)
	return true
}

func (r *Reconciler) finishVerify(id, target, output string, started time.Time, gen int64) {
	defer metrics.SetVerifying(0)
	app, err := r.Registry.Get(id)
	if err != nil || app == nil {
		return
	}
	timeout := 3 * time.Minute
	if raw := strings.TrimSpace(app.Policy().HealthTimeout); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := r.confirmHealthy(ctx, app, target); err != nil {
		_ = r.rollbackUnhealthy(ctx, app, target, output, err, started, gen)
		metrics.ObserveReconcile(app.Name, "rolled_back", "rollback")
		return
	}
	now := time.Now()
	_, _ = r.Registry.FinishSync(app.ID, gen, "synced", now, target, app.LastSeenCommitMessage, output, "")
	metrics.ObserveReconcile(app.Name, "applied", r.currentTrigger(app.ID))
}
