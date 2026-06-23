//go:build windows

package components

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"unsafe"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"golang.org/x/sys/windows"
)

// Tasklist is a Windows-only sensor that reports the list of processes currently
// running on the machine (the equivalent of the `tasklist` command), enumerated
// via the Windows ToolHelp snapshot API.
var Tasklist = resource.NewModel("viam", "windows-diagnostics", "tasklist")

func init() {
	resource.RegisterComponent(sensor.API, Tasklist,
		resource.Registration[sensor.Sensor, *TasklistConfig]{
			Constructor: newWindowsDiagnosticsTasklist,
		},
	)
}

type TasklistConfig struct {
	// NameFilter, when set, restricts reported processes to those whose executable
	// name contains this substring (case-insensitive). Empty reports all processes.
	NameFilter string `json:"name_filter"`
}

func (cfg *TasklistConfig) Validate(path string) ([]string, []string, error) {
	return nil, nil, nil
}

type windowsDiagnosticsTasklist struct {
	resource.AlwaysRebuild
	resource.Named

	name   resource.Name
	logger logging.Logger
	cfg    *TasklistConfig

	cancelCtx  context.Context
	cancelFunc func()

	// CPU% is a rate, so it requires two samples. mu guards the previous-sample
	// state carried between Readings calls: the system-wide CPU time and each
	// process's cumulative kernel+user time, both in 100ns units, keyed by PID.
	mu            sync.Mutex
	prevSystemCPU uint64
	prevProcCPU   map[uint32]uint64
	hasPrev       bool
}

func newWindowsDiagnosticsTasklist(
	ctx context.Context,
	deps resource.Dependencies,
	rawConf resource.Config,
	logger logging.Logger,
) (sensor.Sensor, error) {
	conf, err := resource.NativeConfig[*TasklistConfig](rawConf)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	if conf.NameFilter != "" {
		logger.Infof("Windows tasklist diagnostics filtering processes by %q", conf.NameFilter)
	} else {
		logger.Info("Windows tasklist diagnostics reporting all processes")
	}

	return &windowsDiagnosticsTasklist{
		name:       rawConf.ResourceName(),
		logger:     logger,
		cfg:        conf,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
	}, nil
}

func (s *windowsDiagnosticsTasklist) Name() resource.Name { return s.name }

// Readings enumerates the running processes and returns a count plus one entry
// per process (pid, ppid, name, threads), optionally filtered by name_filter.
func (s *windowsDiagnosticsTasklist) Readings(
	ctx context.Context,
	extra map[string]interface{},
) (map[string]interface{}, error) {
	s.logger.Debug("Tasklist Readings called")

	procs, err := listProcesses(s.logger)
	if err != nil {
		return nil, err
	}

	// Sample system-wide CPU time. Its delta between two Readings equals the total
	// CPU time available across all logical processors over the interval, so using
	// it as the denominator yields a percentage of total capacity (0–100), the same
	// basis Task Manager uses — no separate elapsed-time or CPU-count math needed.
	var idle, kernel, user windows.Filetime
	if err := getSystemTimes(&idle, &kernel, &user); err != nil {
		return nil, err
	}
	systemCPU := filetimeToUint64(kernel) + filetimeToUint64(user)

	cpuPercents := s.computeCPUPercents(procs, systemCPU)

	filter := strings.ToLower(s.cfg.NameFilter)
	processes := make([]interface{}, 0, len(procs))
	for _, p := range procs {
		if filter != "" && !strings.Contains(strings.ToLower(p.name), filter) {
			continue
		}
		processes = append(processes, map[string]interface{}{
			"pid":         p.pid,
			"ppid":        p.ppid,
			"name":        p.name,
			"threads":     p.threads,
			"cpu_percent": cpuPercents[p.pid], // 0 on the first reading (no prior sample)
		})
	}

	s.logger.Debugf("Tasklist reporting %d of %d processes", len(processes), len(procs))

	return map[string]interface{}{
		"process_count": len(processes),
		"processes":     processes,
	}, nil
}

func (s *windowsDiagnosticsTasklist) DoCommand(
	ctx context.Context,
	cmd map[string]interface{},
) (map[string]interface{}, error) {
	return nil, errUnimplemented
}

func (s *windowsDiagnosticsTasklist) Close(context.Context) error {
	s.cancelFunc()
	return nil
}

type processInfo struct {
	pid     uint32
	ppid    uint32
	threads uint32
	name    string
	cpuTime uint64 // cumulative kernel+user CPU time since process start, in 100ns units
}

// listProcesses enumerates running processes using the Windows ToolHelp snapshot
// API (CreateToolhelp32Snapshot + Process32First/Next), the same mechanism the
// `tasklist` command relies on. Results are sorted by PID for stable output.
func listProcesses(logger logging.Logger) ([]processInfo, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		logger.Debugf("CreateToolhelp32Snapshot failed: %v", err)
		return nil, err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	procs := make([]processInfo, 0, 256)

	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		p := processInfo{
			pid:     entry.ProcessID,
			ppid:    entry.ParentProcessID,
			threads: entry.Threads,
			name:    windows.UTF16ToString(entry.ExeFile[:]),
		}
		// CPU time needs a per-process handle. Some processes (e.g. System Idle/PID 0,
		// and protected processes) can't be opened; leave cpuTime at 0 for those.
		if cpuTime, cpuErr := getProcessCPUTime(p.pid); cpuErr != nil {
			logger.Debugf("could not read CPU time for pid %d (%s): %v", p.pid, p.name, cpuErr)
		} else {
			p.cpuTime = cpuTime
		}
		procs = append(procs, p)
		err = windows.Process32Next(snapshot, &entry)
	}

	// ERROR_NO_MORE_FILES marks the natural end of enumeration, not a failure.
	if err != nil && err != windows.ERROR_NO_MORE_FILES {
		logger.Debugf("Process32Next failed: %v", err)
		return nil, err
	}

	sort.Slice(procs, func(i, j int) bool {
		return procs[i].pid < procs[j].pid
	})

	logger.Debugf("Enumerated %d processes", len(procs))
	return procs, nil
}

// computeCPUPercents returns per-PID CPU utilization (0–100, 1 decimal) by diffing
// each process's cumulative CPU time against the previous Readings sample, using the
// system-wide CPU-time delta over the same interval as the denominator. It also
// updates the stored sample for the next call. The first call (and any newly-seen
// PID) returns no entry, so callers see 0.
func (s *windowsDiagnosticsTasklist) computeCPUPercents(procs []processInfo, systemCPU uint64) map[uint32]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	percents := make(map[uint32]float64, len(procs))
	curProc := make(map[uint32]uint64, len(procs))

	deltaSystem := systemCPU - s.prevSystemCPU
	for _, p := range procs {
		curProc[p.pid] = p.cpuTime
		// Only emit a percentage when we have a prior sample for this PID, the system
		// clock advanced, and the cumulative time didn't go backwards (PID reuse).
		if s.hasPrev && deltaSystem > 0 {
			if prev, ok := s.prevProcCPU[p.pid]; ok && p.cpuTime >= prev {
				percents[p.pid] = math.Round(float64(p.cpuTime-prev)/float64(deltaSystem)*100*10) / 10
			}
		}
	}

	s.prevSystemCPU = systemCPU
	s.prevProcCPU = curProc
	s.hasPrev = true
	return percents
}

// getProcessCPUTime opens the process and returns its cumulative kernel+user CPU
// time in 100ns units via GetProcessTimes. PROCESS_QUERY_LIMITED_INFORMATION is the
// least-privileged access that GetProcessTimes accepts and works across user
// boundaries on Vista+.
func getProcessCPUTime(pid uint32) (uint64, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return filetimeToUint64(kernel) + filetimeToUint64(user), nil
}
