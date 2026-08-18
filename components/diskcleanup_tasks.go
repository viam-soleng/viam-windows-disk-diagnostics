//go:build windows

package components

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.viam.com/rdk/logging"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Services that hold handles under SoftwareDistribution. They are stopped in this
// order and restarted in reverse: bits depends on nothing here, but wuauserv will
// restart bits itself if it comes up first, which races the deletion.
//
// usosvc and dosvc also keep files in the cache open on current builds, so leaving
// them running is the difference between mostly-emptied and emptied. They are not
// required, though: both refuse to stop on some builds even as LocalSystem, and a
// partial clean is much better than no clean at all.
var softwareDistributionServices = []struct {
	name     string
	required bool
}{
	{"wuauserv", true},
	{"bits", true},
	{"usosvc", false},
	{"dosvc", false},
}

const serviceStateTimeout = 90 * time.Second

// windowsDir returns the Windows directory (normally C:\Windows). It is queried
// rather than hardcoded because the volume this module cleans is not required to
// be the one the config points at, and Windows is not required to be on C:.
func windowsDir() string {
	dir, err := windows.GetSystemWindowsDirectory()
	if err != nil || dir == "" {
		return `C:\Windows`
	}
	return dir
}

func softwareDistributionDownloadDir() string {
	return filepath.Join(windowsDir(), "SoftwareDistribution", "Download")
}

// tempDirs returns the temp directories to clean: the machine-wide one plus the
// current process's %TEMP%, which differ when viam-server does not run as
// LocalSystem. Duplicates are removed so a directory is never walked twice.
func tempDirs() []string {
	dirs := []string{filepath.Join(windowsDir(), "Temp")}
	if t := os.TempDir(); t != "" && !strings.EqualFold(filepath.Clean(t), filepath.Clean(dirs[0])) {
		dirs = append(dirs, filepath.Clean(t))
	}
	return dirs
}

// deliveryOptimizationDirs returns the known Delivery Optimization cache
// locations. Which one is used varies by Windows build, and a build that does not
// use a given path simply has no directory there.
func deliveryOptimizationDirs() []string {
	win := windowsDir()
	return []string{
		filepath.Join(win, "ServiceProfiles", "NetworkService", "AppData", "Local",
			"Microsoft", "Windows", "DeliveryOptimization"),
		filepath.Join(win, "SoftwareDistribution", "DeliveryOptimization"),
	}
}

// taskDirs returns the directories a task deletes from, and whether the task is
// directory-based at all. DISM and cleanmgr are not, so they cannot be sized or
// cleaned by walking the filesystem.
func (s *windowsDiagnosticsDiskCleanup) taskDirs(name string) ([]string, bool) {
	switch name {
	case taskSoftwareDistribution:
		return []string{softwareDistributionDownloadDir()}, true
	case taskTempFiles:
		return tempDirs(), true
	case taskDeliveryOptimization:
		return deliveryOptimizationDirs(), true
	default:
		return nil, false
	}
}

// --- tasks ---

// taskOutcome is what a cleanup task reports on success. Freed bytes are not part
// of it: only the caller's before/after free-space samples can measure those.
type taskOutcome struct {
	items  int
	detail string
	// rebootRequired is set when the tool did its work but needs a restart to
	// finish. Windows reports that as a distinct success code, not a failure.
	rebootRequired bool
}

// cleanSoftwareDistribution empties the Windows Update payload cache. The update
// services must be stopped first because they hold the downloaded files open;
// they are always restarted, including on failure, since leaving wuauserv down
// would silently stop the machine from patching itself.
func (s *windowsDiagnosticsDiskCleanup) cleanSoftwareDistribution(ctx context.Context) (taskOutcome, error) {
	stopped := make([]string, 0, len(softwareDistributionServices))
	defer func() {
		// Restart in reverse order, and outside ctx: if the deletion timed out we
		// still must put the services back.
		for i := len(stopped) - 1; i >= 0; i-- {
			if err := startService(stopped[i]); err != nil {
				s.logger.Errorf("Failed to restart service %s after cleanup: %v", stopped[i], err)
			} else {
				s.logger.Debugf("Restarted service %s", stopped[i])
			}
		}
	}()

	for _, service := range softwareDistributionServices {
		wasRunning, err := stopService(ctx, service.name, s.logger)
		if wasRunning {
			stopped = append(stopped, service.name)
		}
		if err != nil {
			if service.required {
				return taskOutcome{}, fmt.Errorf("stopping %s: %w", service.name, err)
			}
			// An optional service that will not stop just means some of its files stay
			// locked and land in the skipped count below.
			s.logger.Debugf("Could not stop optional service %s, continuing: %v", service.name, err)
		}
	}

	dir := softwareDistributionDownloadDir()
	// The directory itself is kept and only its contents removed, so the ACL
	// Windows put on it survives; Windows repopulates it on the next update scan.
	removed, kept, failed, err := deleteContents(ctx, dir, 0, s.logger)
	if err != nil {
		return taskOutcome{items: removed}, fmt.Errorf("emptying %s: %w", dir, err)
	}
	return taskOutcome{items: removed, detail: describeRemoval(dir, removed, kept, failed)}, nil
}

// cleanTempFiles empties the temp directories, skipping anything newer than
// tempMinAge. The age filter matters: viam-server unpacks modules through a temp
// directory, so deleting fresh files could pull the floor out from under the very
// process running this cleanup.
func (s *windowsDiagnosticsDiskCleanup) cleanTempFiles(ctx context.Context) (taskOutcome, error) {
	var (
		outcome  taskOutcome
		details  []string
		firstErr error
	)
	for _, dir := range tempDirs() {
		removed, kept, failed, err := deleteContents(ctx, dir, s.tempMinAge, s.logger)
		outcome.items += removed
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("emptying %s: %w", dir, err)
			}
			continue
		}
		details = append(details, describeRemoval(dir, removed, kept, failed))
	}
	// The detail is returned even alongside an error, so a run that cleared one
	// directory and was denied the other still reports what it did clear.
	outcome.detail = strings.Join(details, "; ")
	return outcome, firstErr
}

// cleanDeliveryOptimization drops the peer-to-peer update cache. The built-in
// cmdlet is preferred because it coordinates with the DO service; when it fails or
// is missing the cache directories are emptied directly instead.
func (s *windowsDiagnosticsDiskCleanup) cleanDeliveryOptimization(ctx context.Context) (taskOutcome, error) {
	// PowerShell exits 0 even when a cmdlet writes a non-terminating error, so
	// access-denied would otherwise look like success and skip the fallback below.
	// Promoting errors to terminating and exiting non-zero by hand is what makes the
	// exit code mean what this code assumes it means.
	const script = `$ErrorActionPreference = 'Stop'; ` +
		`try { Delete-DeliveryOptimizationCache -Force -ErrorAction Stop } ` +
		`catch { Write-Error $_; exit 1 }`

	out, err := runCommand(ctx, s.taskTimeout, s.logger, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-Command", script)
	if err == nil {
		return taskOutcome{detail: "Delete-DeliveryOptimizationCache succeeded"}, nil
	}
	s.logger.Debugf("Delete-DeliveryOptimizationCache failed (%v: %s); "+
		"falling back to emptying the cache directories", err, lastLines(out, 3))

	var (
		outcome taskOutcome
		details []string
	)
	for _, dir := range deliveryOptimizationDirs() {
		if _, statErr := os.Stat(dir); statErr != nil {
			continue
		}
		removed, kept, failed, rmErr := deleteContents(ctx, dir, 0, s.logger)
		outcome.items += removed
		if rmErr != nil {
			outcome.detail = strings.Join(details, "; ")
			return outcome, fmt.Errorf("emptying %s: %w", dir, rmErr)
		}
		details = append(details, describeRemoval(dir, removed, kept, failed))
	}
	if len(details) == 0 {
		return taskOutcome{}, fmt.Errorf(
			"Delete-DeliveryOptimizationCache failed and no cache directory was present: %w", err)
	}
	outcome.detail = strings.Join(details, "; ")
	return outcome, nil
}

// runDISMComponentCleanup removes superseded component versions from WinSxS.
// This is the slow task — tens of minutes on a machine that has never been
// cleaned — and is the reason every task runs off the Readings path.
func (s *windowsDiagnosticsDiskCleanup) runDISMComponentCleanup(ctx context.Context) (taskOutcome, error) {
	args := []string{"/Online", "/Cleanup-Image", "/StartComponentCleanup"}
	if s.cfg.DISMResetBase {
		args = append(args, "/ResetBase")
	}
	out, err := runCommand(ctx, s.taskTimeout, s.logger, "Dism.exe", args...)
	if err != nil && !isRebootRequiredExit(err) {
		return taskOutcome{}, fmt.Errorf("DISM %s failed: %w (output: %s)",
			strings.Join(args, " "), err, lastLines(out, 5))
	}
	outcome := taskOutcome{detail: lastLines(out, 3), rebootRequired: err != nil}
	if outcome.rebootRequired {
		outcome.detail += " | a restart is needed to finish the cleanup"
	}
	return outcome, nil
}

// runCleanmgr drives the built-in Disk Cleanup handlers headlessly. cleanmgr has
// no command line for choosing handlers; it reads them from a numbered answer
// file in the registry, so the answer file is written immediately before the run.
func (s *windowsDiagnosticsDiskCleanup) runCleanmgr(ctx context.Context) (taskOutcome, error) {
	enabled, err := s.configureCleanmgrHandlers()
	if err != nil {
		return taskOutcome{}, err
	}
	if enabled == 0 {
		return taskOutcome{}, fmt.Errorf("no cleanmgr handlers matched cleanmgr_handlers %v",
			s.cfg.CleanmgrHandlers)
	}

	out, err := runCommand(ctx, s.taskTimeout, s.logger,
		"cleanmgr.exe", fmt.Sprintf("/sagerun:%d", s.sagesetID))
	if err != nil && !isRebootRequiredExit(err) {
		return taskOutcome{}, fmt.Errorf("cleanmgr /sagerun:%d failed: %w (output: %s)",
			s.sagesetID, err, lastLines(out, 5))
	}

	// The process just launched hands the work to a separate cleanmgr process and
	// exits immediately, so without this wait the task would report a duration and a
	// free-space delta measured against nothing, and the next task would start while
	// cleanmgr was still deleting.
	waited, waitErr := waitForProcessExit(ctx, "cleanmgr", s.logger)
	if waitErr != nil {
		s.logger.Warnf("Stopped waiting for cleanmgr to finish: %v", waitErr)
	}

	// The Windows Update handler is the exception the wait cannot cover: it stages
	// its work and finishes during the next restart.
	detail := fmt.Sprintf(
		"ran %d cleanmgr handlers from sageset %d in %s; the Update Cleanup handler completes on the next reboot",
		enabled, s.sagesetID, waited.Truncate(time.Second))
	return taskOutcome{items: enabled, detail: detail, rebootRequired: true}, nil
}

// configureCleanmgrHandlers writes StateFlagsNNNN under every VolumeCaches handler:
// 2 (enabled) for the handlers this model should run, 0 for the rest, so a handler
// dropped from the config is actually disabled rather than left on from last time.
func (s *windowsDiagnosticsDiskCleanup) configureCleanmgrHandlers() (int, error) {
	const volumeCaches = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\VolumeCaches`

	root, err := registry.OpenKey(registry.LOCAL_MACHINE, volumeCaches, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", volumeCaches, err)
	}
	defer root.Close()

	handlers, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return 0, fmt.Errorf("listing %s: %w", volumeCaches, err)
	}

	allowed := make(map[string]bool, len(s.cfg.CleanmgrHandlers))
	for _, h := range s.cfg.CleanmgrHandlers {
		allowed[strings.ToLower(h)] = true
	}

	valueName := fmt.Sprintf("StateFlags%04d", s.sagesetID)
	enabled := 0
	for _, handler := range handlers {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, volumeCaches+`\`+handler, registry.SET_VALUE)
		if err != nil {
			s.logger.Debugf("Skipping cleanmgr handler %q: %v", handler, err)
			continue
		}
		state := uint32(2)
		if len(allowed) > 0 && !allowed[strings.ToLower(handler)] {
			state = 0
		}
		if err := key.SetDWordValue(valueName, state); err != nil {
			s.logger.Debugf("Could not set %s on cleanmgr handler %q: %v", valueName, handler, err)
		} else if state == 2 {
			enabled++
		}
		key.Close()
	}
	s.logger.Debugf("Enabled %d of %d cleanmgr handlers in sageset %d", enabled, len(handlers), s.sagesetID)
	return enabled, nil
}

// --- filesystem helpers ---

// dirStats returns the total size of a directory tree and the most recent
// modification time in it. Unreadable entries are skipped rather than failing the
// walk: a temp directory almost always contains something held open by another
// process, and that must not make the whole measurement fail.
func dirStats(ctx context.Context, dir string) (uint64, time.Time, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return 0, time.Time{}, nil
		}
		return 0, time.Time{}, err
	}

	var (
		size   uint64
		newest time.Time
	)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries, keep walking
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // entry vanished mid-walk
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		if !d.IsDir() {
			size += uint64(info.Size())
		}
		return nil
	})
	return size, newest, err
}

// deleteContents removes everything inside dir while keeping dir itself, and
// reports three counts: entries removed, entries deliberately kept (too new, or
// too unreadable to age-check), and entries that resisted removal. Entries that
// cannot be removed are skipped rather than aborting the task.
//
// Entries whose newest content is younger than minAge are left alone; passing 0
// deletes everything.
func deleteContents(
	ctx context.Context,
	dir string,
	minAge time.Duration,
	logger logging.Logger,
) (removed, kept, failed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, 0, nil
		}
		return 0, 0, 0, err
	}

	cutoff := time.Now().Add(-minAge)
	for _, entry := range entries {
		if ctx.Err() != nil {
			return removed, kept, failed, ctx.Err()
		}
		path := filepath.Join(dir, entry.Name())

		if minAge > 0 {
			// A directory's own mtime says nothing about how fresh its contents are,
			// so the whole subtree's newest mtime is what the age filter tests.
			_, newest, statErr := dirStats(ctx, path)
			if statErr != nil {
				// Age unknown means the age filter cannot be honored, so keep the entry.
				// This is a deliberate keep, not a failed delete — it is never attempted.
				logger.Debugf("Keeping %s: could not determine its age: %v", path, statErr)
				kept++
				continue
			}
			if newest.After(cutoff) {
				logger.Debugf("Keeping %s: modified %s ago", path, time.Since(newest).Truncate(time.Second))
				kept++
				continue
			}
		}

		if rmErr := os.RemoveAll(path); rmErr != nil {
			logger.Debugf("Could not remove %s: %v", path, rmErr)
			failed++
			continue
		}
		removed++
	}
	return removed, kept, failed, nil
}

func describeRemoval(dir string, removed, kept, failed int) string {
	parts := []string{fmt.Sprintf("%s: removed %d entries", dir, removed)}
	if kept > 0 {
		parts = append(parts, fmt.Sprintf("kept %d too new to delete", kept))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d in use or protected", failed))
	}
	return strings.Join(parts, ", ")
}

// --- process and service helpers ---

// runCommand runs an external tool with a timeout and returns its combined output.
// HideWindow keeps cleanmgr and DISM from flashing a console on an interactive
// desktop; under the viam-server service there is no desktop to flash on.
func runCommand(
	ctx context.Context,
	timeout time.Duration,
	logger logging.Logger,
	name string,
	args ...string,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logger.Debugf("Running %s %s", name, strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))

	if ctxErr := ctx.Err(); ctxErr != nil {
		return text, fmt.Errorf("%s did not finish within %s: %w", name, timeout, ctxErr)
	}
	return text, err
}

// stopService stops a Windows service and waits for it to reach the stopped state,
// reporting whether it had been running so the caller knows to restart it.
func stopService(ctx context.Context, name string, logger logging.Logger) (bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("connecting to the service manager: %w", err)
	}
	defer m.Disconnect()

	service, err := m.OpenService(name)
	if err != nil {
		return false, fmt.Errorf("opening service: %w", err)
	}
	defer service.Close()

	status, err := service.Query()
	if err != nil {
		return false, fmt.Errorf("querying service: %w", err)
	}
	if status.State == svc.Stopped {
		logger.Debugf("Service %s is already stopped", name)
		return false, nil
	}

	if _, err := service.Control(svc.Stop); err != nil {
		return true, fmt.Errorf("sending stop: %w", err)
	}

	// Control returns as soon as the stop is accepted, so the files are not
	// necessarily released yet; poll until the service actually reports stopped.
	deadline := time.Now().Add(serviceStateTimeout)
	for {
		status, err = service.Query()
		if err != nil {
			return true, fmt.Errorf("querying service while stopping: %w", err)
		}
		if status.State == svc.Stopped {
			logger.Debugf("Service %s stopped", name)
			return true, nil
		}
		if time.Now().After(deadline) {
			return true, fmt.Errorf("service did not stop within %s", serviceStateTimeout)
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// startService starts a service, treating an already-running service as success.
func startService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to the service manager: %w", err)
	}
	defer m.Disconnect()

	service, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("opening service: %w", err)
	}
	defer service.Close()

	status, err := service.Query()
	if err == nil && (status.State == svc.Running || status.State == svc.StartPending) {
		return nil
	}
	if err := service.Start(); err != nil {
		// A service Windows already restarted on its own is not a failure.
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return nil
		}
		return err
	}
	return nil
}

// Windows exit codes that mean the operation succeeded but needs a restart to take
// effect. DISM /StartComponentCleanup returns 3010 routinely on a machine that has
// not been cleaned before, and treating it as a failure would report the most common
// good outcome as an error.
const (
	exitRebootRequired  = 3010 // ERROR_SUCCESS_REBOOT_REQUIRED
	exitRebootInitiated = 1641 // ERROR_SUCCESS_REBOOT_INITIATED
)

// isRebootRequiredExit reports whether a command failed only in the sense that
// Windows wants a restart.
func isRebootRequiredExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	switch exitErr.ExitCode() {
	case exitRebootRequired, exitRebootInitiated:
		return true
	default:
		return false
	}
}

// --- Finding 8 support ---

// waitForProcessExit blocks until no process whose name contains match is running,
// returning how long it waited. cleanmgr /sagerun launches a separate process and
// returns immediately, so waiting on the launched process alone proves nothing.
func waitForProcessExit(ctx context.Context, match string, logger logging.Logger) (time.Duration, error) {
	started := time.Now()
	// cleanmgr may not have spawned its worker by the time the launcher exits, so a
	// single empty result is not proof it finished; require two consecutive ones.
	consecutiveEmpty := 0
	for {
		select {
		case <-ctx.Done():
			return time.Since(started), ctx.Err()
		case <-time.After(2 * time.Second):
		}

		procs, err := listProcesses(logger, strings.ToLower(match))
		if err != nil {
			return time.Since(started), err
		}
		if len(procs) > 0 {
			consecutiveEmpty = 0
			continue
		}
		consecutiveEmpty++
		if consecutiveEmpty >= 2 {
			return time.Since(started), nil
		}
	}
}

// isElevated reports whether the process has administrator rights. Every cleanup
// task needs them.
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// isRebootPending reports whether Windows is waiting on a restart. It is worth
// surfacing because servicing operations queue behind a pending reboot, and
// because cleanmgr's Update Cleanup handler only finishes during one.
func isRebootPending(logger logging.Logger) bool {
	keys := []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending`,
		`SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired`,
	}
	for _, path := range keys {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err == nil {
			key.Close()
			return true
		}
	}

	// A queued file rename is the third signal, and unlike the keys above it lives
	// in a value on a key that always exists.
	key, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Session Manager`, registry.QUERY_VALUE)
	if err != nil {
		logger.Debugf("Could not check PendingFileRenameOperations: %v", err)
		return false
	}
	defer key.Close()

	pending, _, err := key.GetStringsValue("PendingFileRenameOperations")
	return err == nil && len(pending) > 0
}

// cleanOutputLines normalizes console output from DISM and cleanmgr into the
// lines a human would actually have seen: progress-bar frames, in-place redraws,
// and blank separators are dropped.
func cleanOutputLines(out string) []string {
	raw := strings.Split(out, "\n")
	cleaned := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimRight(strings.ReplaceAll(line, "\b", ""), " \t\r")
		// A carriage return left inside the line means the tool redrew it in place,
		// so only the text after the last one was ever on screen.
		if i := strings.LastIndex(line, "\r"); i >= 0 {
			line = line[i+1:]
		}
		// Leading whitespace is kept: DISM indents the component-store breakdown
		// under the total it belongs to, so flattening it would lose the hierarchy.
		if trimmed := strings.TrimSpace(line); trimmed == "" || isProgressBar(trimmed) {
			continue
		}
		cleaned = append(cleaned, line)
	}
	return cleaned
}

// isProgressBar matches the frames DISM redraws while it works, which look like
// "[=========  42.3%          ]". Hundreds of them precede every real result line.
func isProgressBar(line string) bool {
	return strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && strings.Contains(line, "%")
}

// lastLines trims verbose tool output down to the tail, which is where DISM and
// cleanmgr put their result. The full output is only ever in debug logs.
func lastLines(out string, n int) string {
	cleaned := cleanOutputLines(out)
	if len(cleaned) > n {
		cleaned = cleaned[len(cleaned)-n:]
	}
	return strings.Join(cleaned, " | ")
}
