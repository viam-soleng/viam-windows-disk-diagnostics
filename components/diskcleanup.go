//go:build windows

package components

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// DiskCleanup is a Windows-only sensor that reports how much space each cleanup
// target is holding and reclaims it on demand (DoCommand) or automatically when
// free space falls below a configured threshold.
//
// Readings() is always non-destructive, so the model is safe to poll and to put
// under data capture; nothing is ever deleted except by an explicit `run` command
// or by a configured auto-cleanup threshold being crossed.
var DiskCleanup = resource.NewModel("viam", "windows-diagnostics", "diskcleanup")

// Cleanup tasks, in the order they are executed. The order is deliberate: the
// cheap, high-yield deletions run first so that a machine that is critically full
// recovers space within seconds, before the slow DISM/cleanmgr passes start.
const (
	// taskSoftwareDistribution empties C:\Windows\SoftwareDistribution\Download,
	// the Windows Update payload cache. This is the single biggest recurring
	// offender and Windows recreates whatever it still needs.
	taskSoftwareDistribution = "software_distribution"
	// taskTempFiles empties C:\Windows\Temp and the service account's %TEMP%,
	// subject to a minimum file age so an in-flight installer is not disturbed.
	taskTempFiles = "temp_files"
	// taskDeliveryOptimization drops the peer-to-peer update cache, which is
	// separate from SoftwareDistribution and can independently reach several GB.
	taskDeliveryOptimization = "delivery_optimization"
	// taskDISMComponentCleanup removes superseded WinSxS component versions.
	// Slow (minutes) but reclaims space nothing else can reach.
	taskDISMComponentCleanup = "dism_component_cleanup"
	// taskCleanmgr runs the built-in Disk Cleanup handlers headlessly. Opt-in:
	// it overlaps the tasks above and its Windows Update handler only finishes
	// during the next (slow) reboot.
	taskCleanmgr = "cleanmgr"
)

var (
	// taskOrder is the canonical execution order; a configured `tasks` list is
	// sorted into it rather than honored as written.
	taskOrder = []string{
		taskSoftwareDistribution,
		taskTempFiles,
		taskDeliveryOptimization,
		taskDISMComponentCleanup,
		taskCleanmgr,
	}

	// defaultTasks omits cleanmgr; see taskCleanmgr.
	defaultTasks = []string{
		taskSoftwareDistribution,
		taskTempFiles,
		taskDeliveryOptimization,
		taskDISMComponentCleanup,
	}
)

const (
	defaultTempMinAge  = 24 * time.Hour
	defaultMinInterval = 24 * time.Hour
	defaultTaskTimeout = 45 * time.Minute
	defaultEstimateTTL = 10 * time.Minute
	// defaultSagesetID is the cleanmgr answer-file slot this model owns. It is
	// deliberately not 1, which is the slot an administrator most often configures
	// by hand and which we would otherwise overwrite.
	defaultSagesetID = 99
)

func init() {
	resource.RegisterComponent(sensor.API, DiskCleanup,
		resource.Registration[sensor.Sensor, *DiskCleanupConfig]{
			Constructor: newWindowsDiagnosticsDiskCleanup,
		},
	)
}

type DiskCleanupConfig struct {
	// Path is the volume whose free space is reported and threshold-checked.
	Path string `json:"path"`

	// Tasks selects which cleanup tasks run. Empty means defaultTasks.
	Tasks []string `json:"tasks"`

	// DISMResetBase adds /ResetBase to the component cleanup, which reclaims more
	// space but makes every already-installed update permanently un-uninstallable.
	DISMResetBase bool `json:"dism_reset_base"`

	// TempMinAgeHours is the minimum age a temp file must have before temp_files
	// will delete it. Pointer so an explicit 0 (delete everything) is honored.
	TempMinAgeHours *float64 `json:"temp_min_age_hours"`

	// CleanmgrSagesetID is the StateFlagsNNNN slot cleanmgr runs from (0-9999).
	CleanmgrSagesetID *int `json:"cleanmgr_sageset_id"`

	// CleanmgrHandlers restricts cleanmgr to these VolumeCaches handler names
	// (e.g. "Update Cleanup"). Empty enables every handler on the machine.
	CleanmgrHandlers []string `json:"cleanmgr_handlers"`

	// AutoCleanupBelowFreePercent triggers a cleanup when free space on Path drops
	// below this percentage. 0 disables the percentage trigger.
	AutoCleanupBelowFreePercent float64 `json:"auto_cleanup_below_free_percent"`

	// AutoCleanupBelowFreeBytes triggers a cleanup when free space on Path drops
	// below this many bytes. 0 disables the absolute trigger.
	AutoCleanupBelowFreeBytes uint64 `json:"auto_cleanup_below_free_bytes"`

	// MinCleanupIntervalHours rate-limits automatic cleanups. Pointer so an
	// explicit 0 (no rate limit) is honored.
	MinCleanupIntervalHours *float64 `json:"min_cleanup_interval_hours"`

	// TaskTimeoutSeconds bounds each individual task.
	TaskTimeoutSeconds int `json:"task_timeout_seconds"`

	// EstimateTTLSeconds is how long a reclaimable-space estimate is reused before
	// the directories are re-measured in the background.
	EstimateTTLSeconds int `json:"estimate_ttl_seconds"`
}

// Validate performs validation ONLY.
// Do NOT mutate config here — mutations are discarded by Viam.
func (cfg *DiskCleanupConfig) Validate(path string) ([]string, []string, error) {
	for _, t := range cfg.Tasks {
		if !validTask(t) {
			return nil, nil, fmt.Errorf("unknown task %q; valid tasks are %s",
				t, strings.Join(taskOrder, ", "))
		}
	}
	if cfg.AutoCleanupBelowFreePercent < 0 || cfg.AutoCleanupBelowFreePercent > 100 {
		return nil, nil, fmt.Errorf(
			"auto_cleanup_below_free_percent must be between 0 and 100, got %v",
			cfg.AutoCleanupBelowFreePercent)
	}
	if cfg.CleanmgrSagesetID != nil && (*cfg.CleanmgrSagesetID < 0 || *cfg.CleanmgrSagesetID > 9999) {
		return nil, nil, fmt.Errorf("cleanmgr_sageset_id must be between 0 and 9999, got %d",
			*cfg.CleanmgrSagesetID)
	}
	if cfg.TempMinAgeHours != nil && *cfg.TempMinAgeHours < 0 {
		return nil, nil, errors.New("temp_min_age_hours cannot be negative")
	}
	if cfg.MinCleanupIntervalHours != nil && *cfg.MinCleanupIntervalHours < 0 {
		return nil, nil, errors.New("min_cleanup_interval_hours cannot be negative")
	}
	if cfg.TaskTimeoutSeconds < 0 {
		return nil, nil, errors.New("task_timeout_seconds cannot be negative")
	}
	if cfg.EstimateTTLSeconds < 0 {
		return nil, nil, errors.New("estimate_ttl_seconds cannot be negative")
	}
	return nil, nil, nil
}

func validTask(name string) bool {
	for _, t := range taskOrder {
		if t == name {
			return true
		}
	}
	return false
}

// taskResult records the outcome of one task within a run.
type taskResult struct {
	Name         string
	Status       string // "ok", "error", or "skipped"
	FreedBytes   uint64 // measured as the volume's free-space delta across the task
	ItemsRemoved int
	// RebootRequired marks a task that did its work but needs a restart to finish.
	RebootRequired bool
	Detail         string
	Seconds        float64
}

// cleanupRun records the outcome of one complete cleanup pass.
type cleanupRun struct {
	Trigger    string // "manual" or "auto"
	StartedAt  time.Time
	FinishedAt time.Time
	FreeBefore uint64
	FreeAfter  uint64
	// FreeMeasured is false when either free-space sample failed, which makes the
	// before/after delta meaningless.
	FreeMeasured bool
	Tasks        []taskResult
	Err          string
}

// freedBytes reports the volume-level space reclaimed, or 0 when either endpoint
// of the measurement is missing.
func (run *cleanupRun) freedBytes() uint64 {
	if !run.FreeMeasured {
		return 0
	}
	return saturatingSub(run.FreeAfter, run.FreeBefore)
}

// componentStoreAnalysis caches one DISM /AnalyzeComponentStore result. The
// analysis takes minutes on a slow machine — far longer than a DoCommand caller's
// deadline — so it is run in the background and collected by a later call.
type componentStoreAnalysis struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Lines      []string
	// Recommended is nil when DISM did not print a recommendation line, which is
	// different from it having printed "No".
	Recommended *bool
	Err         string
}

type windowsDiagnosticsDiskCleanup struct {
	resource.AlwaysRebuild
	resource.Named

	logger logging.Logger
	cfg    *DiskCleanupConfig

	// Resolved configuration, computed once at construction.
	path        string
	tasks       []string
	tempMinAge  time.Duration
	minInterval time.Duration
	taskTimeout time.Duration
	estimateTTL time.Duration
	sagesetID   int

	cancelCtx  context.Context
	cancelFunc func()
	done       chan struct{} // closed when any in-flight cleanup goroutine exits

	// mu guards everything below. Cleanup runs and estimate refreshes happen on
	// background goroutines, so Readings only ever reads snapshots under the lock.
	mu           sync.Mutex
	running      bool
	runningSince time.Time
	runningTask  string
	runningTasks []string
	last         *cleanupRun
	estimate     map[string]uint64
	estimatedAt  time.Time
	estimating   bool
	analyzing    bool
	analysisDone chan struct{}
	analysis     *componentStoreAnalysis
}

func newWindowsDiagnosticsDiskCleanup(
	ctx context.Context,
	deps resource.Dependencies,
	rawConf resource.Config,
	logger logging.Logger,
) (sensor.Sensor, error) {
	conf, err := resource.NativeConfig[*DiskCleanupConfig](rawConf)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	s := &windowsDiagnosticsDiskCleanup{
		Named:       rawConf.ResourceName().AsNamed(),
		logger:      logger,
		cfg:         conf,
		path:        normalizeDiskPath(orDefaultString(conf.Path, defaultDiskPath)),
		tasks:       resolveTasks(conf.Tasks),
		tempMinAge:  durationOrDefaultHours(conf.TempMinAgeHours, defaultTempMinAge),
		minInterval: durationOrDefaultHours(conf.MinCleanupIntervalHours, defaultMinInterval),
		taskTimeout: durationOrDefaultSeconds(conf.TaskTimeoutSeconds, defaultTaskTimeout),
		estimateTTL: durationOrDefaultSeconds(conf.EstimateTTLSeconds, defaultEstimateTTL),
		sagesetID:   intOrDefault(conf.CleanmgrSagesetID, defaultSagesetID),
		cancelCtx:   cancelCtx,
		cancelFunc:  cancelFunc,
		done:        make(chan struct{}),
	}
	close(s.done) // nothing in flight yet

	logger.Infof("Windows disk cleanup watching %q with tasks [%s]",
		s.path, strings.Join(s.tasks, ", "))

	// Every task needs administrator rights. viam-server installed as a Windows
	// service runs as LocalSystem and has them; a hand-launched viam-server in a
	// normal shell does not, and would fail one task at a time with obscure
	// access-denied errors, so say it once up front instead.
	if !isElevated() {
		logger.Warn("viam-server is not running elevated; disk cleanup tasks will fail " +
			"with access-denied. Run viam-server as a service or as administrator.")
	}
	if s.cfg.DISMResetBase {
		logger.Warn("dism_reset_base is enabled: installed Windows updates will no longer " +
			"be uninstallable after a component cleanup")
	}

	return s, nil
}

// Readings reports free space on the watched volume, the current per-task
// reclaimable estimate, and the outcome of the last cleanup. It never deletes
// anything itself; it only starts a background cleanup when a configured
// auto-cleanup threshold is crossed.
func (s *windowsDiagnosticsDiskCleanup) Readings(
	ctx context.Context,
	extra map[string]interface{},
) (map[string]interface{}, error) {
	total, free, _, err := getDiskUsage(s.path, s.logger)
	if err != nil {
		return nil, err
	}

	freePercent := 0.0
	if total > 0 {
		freePercent = math.Round(float64(free)/float64(total)*100*10) / 10
	}

	s.maybeRefreshEstimate()

	readings := map[string]interface{}{
		"path":            s.path,
		"total_bytes":     total,
		"free_bytes":      free,
		"used_bytes":      total - free,
		"free_percent":    freePercent,
		"used_percent":    math.Round((100-freePercent)*10) / 10,
		"tasks":           toInterfaceSlice(s.tasks),
		"elevated":        isElevated(),
		"reboot_required": isRebootPending(s.logger),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.addEstimateReadingsLocked(readings)
	s.addRunReadingsLocked(readings)

	// Auto-cleanup is evaluated here rather than on a timer so that it is driven by
	// the same polling interval the operator already configured for this sensor.
	if reason, ok := s.autoCleanupReasonLocked(free, freePercent); ok {
		s.logger.Warnf("Starting automatic disk cleanup on %s: %s", s.path, reason)
		s.startRunLocked("auto", s.tasks)
		readings["cleanup_running"] = true
		readings["cleanup_trigger_reason"] = reason
	}

	return readings, nil
}

func (s *windowsDiagnosticsDiskCleanup) addEstimateReadingsLocked(readings map[string]interface{}) {
	if s.estimatedAt.IsZero() {
		// The first estimate is still being measured in the background.
		readings["reclaimable_estimate_ready"] = false
		return
	}
	var totalReclaimable uint64
	byTask := make(map[string]interface{}, len(s.estimate))
	for name, size := range s.estimate {
		byTask[name] = size
		totalReclaimable += size
	}
	readings["reclaimable_estimate_ready"] = true
	readings["reclaimable_bytes"] = totalReclaimable
	readings["reclaimable_by_task"] = byTask
	readings["reclaimable_estimate_age_seconds"] = math.Round(time.Since(s.estimatedAt).Seconds())
}

func (s *windowsDiagnosticsDiskCleanup) addRunReadingsLocked(readings map[string]interface{}) {
	readings["cleanup_running"] = s.running
	if s.running {
		readings["cleanup_current_task"] = s.runningTask
		readings["cleanup_elapsed_seconds"] = math.Round(time.Since(s.runningSince).Seconds())
	}
	if s.last == nil {
		return
	}
	for k, v := range runSummary(s.last) {
		readings["last_"+k] = v
	}
}

// autoCleanupReasonLocked reports whether a threshold is crossed and a run is
// currently permitted, returning a human-readable reason for the log and readings.
func (s *windowsDiagnosticsDiskCleanup) autoCleanupReasonLocked(free uint64, freePercent float64) (string, bool) {
	if s.running {
		return "", false
	}
	var reason string
	switch {
	case s.cfg.AutoCleanupBelowFreePercent > 0 && freePercent < s.cfg.AutoCleanupBelowFreePercent:
		reason = fmt.Sprintf("free space %.1f%% is below the %.1f%% threshold",
			freePercent, s.cfg.AutoCleanupBelowFreePercent)
	case s.cfg.AutoCleanupBelowFreeBytes > 0 && free < s.cfg.AutoCleanupBelowFreeBytes:
		reason = fmt.Sprintf("free space %d bytes is below the %d byte threshold",
			free, s.cfg.AutoCleanupBelowFreeBytes)
	default:
		return "", false
	}

	// Rate-limit against the last run whatever its trigger, so a manual run also
	// suppresses an immediate automatic one. Without this, a machine that is full
	// for a reason cleanup cannot fix would run DISM on every poll forever.
	if s.last != nil && s.minInterval > 0 {
		if since := time.Since(s.last.StartedAt); since < s.minInterval {
			s.logger.Debugf("Auto-cleanup suppressed (%s): last run was %s ago, "+
				"minimum interval is %s", reason, since.Truncate(time.Second), s.minInterval)
			return "", false
		}
	}
	return reason, true
}

func (s *windowsDiagnosticsDiskCleanup) Close(context.Context) error {
	s.cancelFunc()

	s.mu.Lock()
	done := s.done
	s.mu.Unlock()

	// Cancelling cancelCtx kills any child process (DISM, cleanmgr) and unblocks the
	// run goroutine, but a file deletion already in progress still has to finish, so
	// give it a bounded moment to unwind rather than blocking reconfiguration.
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		s.logger.Warn("Timed out waiting for the in-flight disk cleanup to stop")
	}
	return nil
}

// --- config helpers ---

// resolveTasks turns a configured task list into the set to run, substituting the
// default set when the config omitted the field entirely.
func resolveTasks(configured []string) []string {
	if len(configured) == 0 {
		return defaultTasks
	}
	return orderTasks(configured)
}

// orderTasks de-duplicates a task list and puts it in taskOrder. An empty list
// stays empty — only resolveTasks applies the default set.
func orderTasks(configured []string) []string {
	seen := make(map[string]bool, len(configured))
	for _, t := range configured {
		seen[t] = true
	}
	tasks := make([]string, 0, len(seen))
	for _, t := range taskOrder {
		if seen[t] {
			tasks = append(tasks, t)
		}
	}
	return tasks
}

func orDefaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func intOrDefault(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func durationOrDefaultHours(hours *float64, def time.Duration) time.Duration {
	if hours == nil {
		return def
	}
	return time.Duration(*hours * float64(time.Hour))
}

func durationOrDefaultSeconds(seconds int, def time.Duration) time.Duration {
	if seconds <= 0 {
		return def
	}
	return time.Duration(seconds) * time.Second
}

func toInterfaceSlice(in []string) []interface{} {
	out := make([]interface{}, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

func sortedKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
