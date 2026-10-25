package controller

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/valthaide/conops-private/internal/imagegc"
	"github.com/valthaide/conops-private/internal/metrics"
)

func (r *Reconciler) maybeGC() {
	if !imageGCEnabled() {
		return
	}
	if !r.gcDue() {
		return
	}
	images, err := RunImageGC(r.Registry, r.Executor, imageGCDry())
	if err != nil && r.Logger != nil {
		r.Logger.Warn("image gc failed", "error", err)
		return
	}
	if r.Logger != nil && len(images) > 0 {
		r.Logger.Info("image gc", "dry_run", imageGCDry(), "images", len(images))
	}
}

func (r *Reconciler) gcDue() bool {
	if diskAboveGCThreshold() {
		return true
	}
	interval := time.Hour
	if raw := strings.TrimSpace(os.Getenv("CONOPS_IMAGE_GC_INTERVAL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			interval = parsed
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lastGC.IsZero() && time.Since(r.lastGC) < interval {
		return false
	}
	r.lastGC = time.Now()
	return true
}

func imageGCEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CONOPS_IMAGE_GC")), "on")
}

func imageGCDry() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CONOPS_IMAGE_GC_DRY_RUN")), "true")
}

func imageGCKeep() int {
	keep := 3
	if raw := strings.TrimSpace(os.Getenv("CONOPS_IMAGE_GC_KEEP")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			keep = n
		}
	}
	return keep
}

func imageGCMinAge() time.Duration {
	age := 72 * time.Hour
	if raw := strings.TrimSpace(os.Getenv("CONOPS_IMAGE_GC_MIN_AGE")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			age = parsed
		}
	}
	return age
}

func diskAboveGCThreshold() bool {
	raw := strings.TrimSpace(os.Getenv("CONOPS_IMAGE_GC_DISK_PCT"))
	if raw == "" {
		return false
	}
	pct, err := strconv.Atoi(raw)
	if err != nil || pct <= 0 {
		return false
	}
	path := strings.TrimSpace(os.Getenv("CONOPS_IMAGE_GC_DISK_PATH"))
	if path == "" {
		path = "/var/lib/docker"
	}
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil || st.Blocks == 0 {
		return false
	}
	used := int((st.Blocks - st.Bavail) * 100 / st.Blocks)
	return used >= pct
}

type imageLister interface {
	LocalImages(ctx context.Context) ([]imagegc.Image, error)
	RemoveImage(ctx context.Context, id string) error
}

// RunImageGC removes unused ConOps images that are older than the minimum age and not in the kept revisions.
func RunImageGC(reg *Registry, exec interface{}, dry bool) ([]imagegc.Image, error) {
	lister, ok := exec.(imageLister)
	if !ok || reg == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	images, err := lister.LocalImages(ctx)
	if err != nil {
		return nil, err
	}
	protected, own := protectedImageSets(reg)
	victims := imagegc.Select(images, protected, own, imageGCMinAge(), time.Now())
	if dry {
		return victims, nil
	}
	var removed []imagegc.Image
	for _, image := range victims {
		if err := lister.RemoveImage(ctx, image.ID); err != nil {
			continue
		}
		removed = append(removed, image)
		metrics.ImageGCRemoved.Inc()
	}
	return removed, nil
}

func protectedImageSets(reg *Registry) (map[string]struct{}, map[string]struct{}) {
	protected := map[string]struct{}{}
	own := map[string]struct{}{}
	if reg == nil {
		return protected, own
	}
	for _, app := range reg.List() {
		if app == nil {
			continue
		}
		revs, err := reg.ListRevisions(app.ID, imageGCKeep())
		if err != nil {
			continue
		}
		for _, rev := range revs {
			var images map[string]string
			if json.Unmarshal([]byte(rev.ImagesJSON), &images) != nil {
				continue
			}
			for _, ref := range images {
				ref = strings.TrimSpace(ref)
				if ref == "" {
					continue
				}
				protected[ref] = struct{}{}
				if i := strings.LastIndex(ref, "@"); i >= 0 {
					protected[ref[i+1:]] = struct{}{}
				}
				if repo := imageRepo(ref); repo != "" {
					own[repo] = struct{}{}
				}
			}
		}
	}
	return protected, own
}

func imageRepo(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > 0 && !strings.Contains(ref[i:], "/") {
		ref = ref[:i]
	}
	if strings.HasPrefix(ref, "sha256:") {
		return ""
	}
	return ref
}
