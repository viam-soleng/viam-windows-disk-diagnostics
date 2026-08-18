//go:build windows

package components

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// DoCommand exposes the destructive half of this model. Supported commands:
//
//	{"command": "run"}      start a cleanup; accepts "tasks" and "wait"
//	{"command": "status"}   report the in-flight and last completed run
//	{"command": "estimate"} re-measure reclaimable space now and return it
//	{"command": "analyze"}  run DISM /AnalyzeComponentStore and return its verdict
func (s *windowsDiagnosticsDiskCleanup) DoCommand(
	ctx context.Context,
	cmd map[string]interface{},
) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "run", "run_cleanup", "cleanup":
		return s.doRun(ctx, cmd)
	case "status":
		return s.doStatus(), nil
	case "estimate":
		return s.doEstimate(ctx)
	case "analyze", "analyze_component_store":
		return s.doAnalyze(ctx)
	case "":
		return nil, fmt.Errorf("missing %q key; expected one of: run, status, estimate, analyze", "command")
	default:
		return nil, fmt.Errorf("unknown command %q; expected one of: run, status, estimate, analyze", command)
	}
}

func (s *windowsDiagnosticsDiskCleanup) doRun(
	ctx context.Context,
	cmd map[string]interface{},
) (map[string]interface{}, error) {
	tasks := s.tasks
	if raw, ok := cmd["tasks"]; ok {
		parsed, err := parseTaskList(raw)
		if err != nil {
			return nil, err
		}
		tasks = parsed
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no cleanup tasks selected")
	}

	wait, _ := cmd["wait"].(bool)

	s.mu.Lock()
	if s.running {
		current := s.runningTask
		s.mu.Unlock()
		return map[string]interface{}{
			"started":      false,
			"reason":       "a cleanup is already running",
			"current_task": current,
		}, nil
	}
	// min_cleanup_interval_hours deliberately does not apply here: asking for a
	// cleanup by hand is itself the signal that one is wanted now.
	finished := s.startRunLocked("manual", tasks)
	s.mu.Unlock()

	if !wait {
		return map[string]interface{}{
			"started": true,
			"tasks":   toInterfaceSlice(tasks),
			"waited":  false,
		}, nil
	}

	// "wait": true blocks the caller until the run finishes, which is what a
	// scripted remediation wants. The caller's own deadline still applies.
	select {
	case <-finished:
	case <-ctx.Done():
		return map[string]interface{}{
			"started":  true,
			"waited":   true,
			"complete": false,
			"reason":   "caller context ended before the cleanup finished; it continues in the background",
		}, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[string]interface{}{"started": true, "waited": true, "complete": true}
	for k, v := range runSummary(s.last) {
		result[k] = v
	}
	return result, nil
}

func (s *windowsDiagnosticsDiskCleanup) doStatus() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := map[string]interface{}{
		"running":          s.running,
		"configured_tasks": toInterfaceSlice(s.tasks),
		"elevated":         isElevated(),
	}
	if s.running {
		// The running set can differ from the configured one, because a manual run may
		// override it; reporting s.tasks here would misstate what is actually happening.
		status["tasks"] = toInterfaceSlice(s.runningTasks)
		status["current_task"] = s.runningTask
		status["elapsed_seconds"] = math.Round(time.Since(s.runningSince).Seconds())
	}
	if s.last != nil {
		status["last_run"] = runSummary(s.last)
	}
	return status
}

func (s *windowsDiagnosticsDiskCleanup) doEstimate(ctx context.Context) (map[string]interface{}, error) {
	estimate := s.measureEstimate(ctx)

	s.mu.Lock()
	s.estimate = estimate
	s.estimatedAt = time.Now()
	s.mu.Unlock()

	var total uint64
	byTask := make(map[string]interface{}, len(estimate))
	for _, name := range sortedKeys(estimate) {
		byTask[name] = estimate[name]
		total += estimate[name]
	}
	return map[string]interface{}{
		"reclaimable_bytes":   total,
		"reclaimable_by_task": byTask,
	}, nil
}

func (s *windowsDiagnosticsDiskCleanup) doAnalyze(ctx context.Context) (map[string]interface{}, error) {
	out, err := runCommand(ctx, s.taskTimeout, s.logger,
		"Dism.exe", "/Online", "/Cleanup-Image", "/AnalyzeComponentStore")
	// DISM emits hundreds of progress-bar frames before the report; without this the
	// caller gets several KB of "[==== 42.3% ]" wrapped around a dozen useful lines.
	lines := cleanOutputLines(out)
	if err != nil {
		return nil, fmt.Errorf("DISM /AnalyzeComponentStore failed: %w (output: %s)",
			err, strings.Join(lines, " | "))
	}

	result := map[string]interface{}{"output": strings.Join(lines, "\n")}
	// DISM prints a recommendation line; surface it directly so an operator does not
	// have to parse the whole report to decide whether a cleanup is worth running.
	for _, line := range lines {
		if strings.Contains(line, "Cleanup Recommended") {
			if _, value, found := strings.Cut(line, ":"); found {
				result["cleanup_recommended"] = strings.EqualFold(strings.TrimSpace(value), "yes")
			}
		}
	}
	return result, nil
}

// startRunLocked launches a cleanup on a background goroutine and returns a channel
// closed when it finishes. Callers must hold s.mu and must have checked s.running.
func (s *windowsDiagnosticsDiskCleanup) startRunLocked(trigger string, tasks []string) <-chan struct{} {
	finished := make(chan struct{})
	s.running = true
	s.runningSince = time.Now()
	s.runningTask = ""
	s.runningTasks = tasks
	s.done = finished

	go func() {
		defer close(finished)
		run := s.executeRun(trigger, tasks)

		s.mu.Lock()
		s.running = false
		s.runningTask = ""
		s.runningTasks = nil
		s.last = run
		// Deletions invalidate the cached sizes; force the next Readings to re-measure.
		s.estimatedAt = time.Time{}
		s.mu.Unlock()
	}()

	return finished
}

// executeRun runs each task in order against s.cancelCtx, attributing freed space
// to a task by the volume's free-space delta across it. That is the only figure
// available for DISM and cleanmgr, and it keeps every task on the same basis.
func (s *windowsDiagnosticsDiskCleanup) executeRun(trigger string, tasks []string) *cleanupRun {
	run := &cleanupRun{Trigger: trigger, StartedAt: time.Now()}

	freeBefore, err := s.freeBytes()
	if err != nil {
		s.logger.Warnf("Could not read free space before cleanup: %v", err)
	}
	run.FreeBefore = freeBefore
	// Without a trustworthy baseline there is nothing to subtract from, and reporting
	// the volume's whole free space as reclaimed would be worse than reporting nothing.
	run.FreeMeasured = err == nil

	s.logger.Infof("Disk cleanup (%s) starting on %s with %d bytes free", trigger, s.path, freeBefore)

	for _, name := range tasks {
		if s.cancelCtx.Err() != nil {
			run.Err = "cleanup cancelled"
			s.logger.Warn("Disk cleanup cancelled before task " + name)
			break
		}

		s.mu.Lock()
		s.runningTask = name
		s.mu.Unlock()

		result := s.executeTask(name)
		run.Tasks = append(run.Tasks, result)

		if result.Status == "error" {
			// One failing task must not abort the rest: the tasks are independent, and
			// the cheap ones that already succeeded are exactly what keeps the machine
			// alive when the slow ones are broken.
			s.logger.Warnf("Disk cleanup task %s failed: %s", name, result.Detail)
		} else {
			s.logger.Infof("Disk cleanup task %s %s in %.0fs, freed %d bytes",
				name, result.Status, result.Seconds, result.FreedBytes)
		}
	}

	freeAfter, err := s.freeBytes()
	if err != nil {
		s.logger.Warnf("Could not read free space after cleanup: %v", err)
		freeAfter = run.FreeBefore
		run.FreeMeasured = false
	}
	run.FreeAfter = freeAfter
	run.FinishedAt = time.Now()

	s.logger.Infof("Disk cleanup (%s) finished in %s, reclaimed %d bytes (%d free)",
		trigger, run.FinishedAt.Sub(run.StartedAt).Truncate(time.Second),
		run.freedBytes(), freeAfter)

	return run
}

func (s *windowsDiagnosticsDiskCleanup) executeTask(name string) taskResult {
	result := taskResult{Name: name, Status: "ok"}
	started := time.Now()

	freeBefore, beforeErr := s.freeBytes()

	ctx, cancel := context.WithTimeout(s.cancelCtx, s.taskTimeout)
	defer cancel()

	var (
		outcome taskOutcome
		err     error
	)
	switch name {
	case taskSoftwareDistribution:
		outcome, err = s.cleanSoftwareDistribution(ctx)
	case taskTempFiles:
		outcome, err = s.cleanTempFiles(ctx)
	case taskDeliveryOptimization:
		outcome, err = s.cleanDeliveryOptimization(ctx)
	case taskDISMComponentCleanup:
		outcome, err = s.runDISMComponentCleanup(ctx)
	case taskCleanmgr:
		outcome, err = s.runCleanmgr(ctx)
	default:
		err = fmt.Errorf("unknown task %q", name)
	}

	freeAfter, afterErr := s.freeBytes()

	result.ItemsRemoved = outcome.items
	result.Detail = outcome.detail
	result.RebootRequired = outcome.rebootRequired
	result.Seconds = math.Round(time.Since(started).Seconds())
	// Attribute freed space only when both samples are trustworthy; a failed read
	// returns 0, which would otherwise be reported as the whole volume reclaimed.
	if beforeErr == nil && afterErr == nil {
		result.FreedBytes = saturatingSub(freeAfter, freeBefore)
	}
	if err != nil {
		result.Status = "error"
		// Keep whatever the task managed to do before it failed: "removed 812 entries;
		// emptying C:\\Windows\\Temp: access denied" tells an operator far more than
		// the error alone, which reads as if nothing happened.
		if outcome.detail != "" {
			result.Detail = outcome.detail + "; " + err.Error()
		} else {
			result.Detail = err.Error()
		}
	}
	return result
}

// maybeRefreshEstimate measures reclaimable space on a background goroutine when
// the cached figure is stale. It is deliberately never measured inline: walking
// C:\Windows\Temp can take seconds, and Readings must stay cheap enough to poll.
func (s *windowsDiagnosticsDiskCleanup) maybeRefreshEstimate() {
	s.mu.Lock()
	fresh := !s.estimatedAt.IsZero() && time.Since(s.estimatedAt) < s.estimateTTL
	// Sizes measured during a cleanup would be meaningless, and a second walk while
	// one is already in flight would just duplicate the I/O.
	if fresh || s.estimating || s.running {
		s.mu.Unlock()
		return
	}
	s.estimating = true
	s.mu.Unlock()

	go func() {
		estimate := s.measureEstimate(s.cancelCtx)
		s.mu.Lock()
		s.estimate = estimate
		s.estimatedAt = time.Now()
		s.estimating = false
		s.mu.Unlock()
	}()
}

// measureEstimate reports the bytes each enabled directory-based task is holding.
// DISM and cleanmgr are absent: neither can be sized without running a multi-minute
// analysis, so they are reported on demand via the "analyze" command instead.
func (s *windowsDiagnosticsDiskCleanup) measureEstimate(ctx context.Context) map[string]uint64 {
	estimate := make(map[string]uint64, len(s.tasks))
	for _, name := range s.tasks {
		dirs, ok := s.taskDirs(name)
		if !ok {
			continue
		}
		var total uint64
		for _, dir := range dirs {
			size, _, err := dirStats(ctx, dir)
			if err != nil {
				s.logger.Debugf("Could not size %s: %v", dir, err)
				continue
			}
			total += size
		}
		estimate[name] = total
	}
	return estimate
}

func parseTaskList(raw interface{}) ([]string, error) {
	// DoCommand arguments arrive as protobuf structs, so a JSON array is
	// []interface{} of strings rather than []string.
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("\"tasks\" must be an array of task names, got %T", raw)
	}
	tasks := make([]string, 0, len(list))
	for _, item := range list {
		name, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("\"tasks\" must contain strings, got %T", item)
		}
		if !validTask(name) {
			return nil, fmt.Errorf("unknown task %q; valid tasks are %s",
				name, strings.Join(taskOrder, ", "))
		}
		tasks = append(tasks, name)
	}
	// orderTasks, not resolveTasks: an explicitly empty array must stay empty so the
	// caller's guard rejects it. Falling back to the default set here would turn
	// "run nothing" into "run everything, including DISM".
	return orderTasks(tasks), nil
}

// runSummary flattens a run for readings and DoCommand responses. Values stay in
// protobuf-representable types (no time.Time, no []taskResult).
func runSummary(run *cleanupRun) map[string]interface{} {
	if run == nil {
		return nil
	}
	tasks := make([]interface{}, 0, len(run.Tasks))
	for _, t := range run.Tasks {
		tasks = append(tasks, map[string]interface{}{
			"name":             t.Name,
			"status":           t.Status,
			"freed_bytes":      t.FreedBytes,
			"items_removed":    t.ItemsRemoved,
			"reboot_required":  t.RebootRequired,
			"duration_seconds": t.Seconds,
			"detail":           t.Detail,
		})
	}
	summary := map[string]interface{}{
		"trigger":          run.Trigger,
		"started_at":       run.StartedAt.UTC().Format(time.RFC3339),
		"finished_at":      run.FinishedAt.UTC().Format(time.RFC3339),
		"duration_seconds": math.Round(run.FinishedAt.Sub(run.StartedAt).Seconds()),
		"freed_bytes":      run.freedBytes(),
		"free_before":      run.FreeBefore,
		"free_after":       run.FreeAfter,
		"free_measured":    run.FreeMeasured,
		"tasks":            tasks,
	}
	if run.Err != "" {
		summary["error"] = run.Err
	}
	return summary
}

// freeBytes reports free space on the watched volume.
func (s *windowsDiagnosticsDiskCleanup) freeBytes() (uint64, error) {
	_, free, _, err := getDiskUsage(s.path, s.logger)
	if err != nil {
		return 0, err
	}
	return free, nil
}

// saturatingSub returns a-b, or 0 if b is larger. Free space can legitimately go
// down across a cleanup if another process wrote more than the cleanup reclaimed,
// and a negative "freed" figure would just be noise.
func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
