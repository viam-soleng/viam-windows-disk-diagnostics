//go:build windows

package components

import (
	"context"
	"math"
	"sync"
	"unsafe"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"golang.org/x/sys/windows"
)

var procGetSystemTimes = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetSystemTimes")

func getSystemTimes(idle, kernel, user *windows.Filetime) error {
	r, _, e := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(idle)),
		uintptr(unsafe.Pointer(kernel)),
		uintptr(unsafe.Pointer(user)),
	)
	if r == 0 {
		return e
	}
	return nil
}

var CPU = resource.NewModel("viam", "windows-diagnostics", "cpu")

func init() {
	resource.RegisterComponent(sensor.API, CPU,
		resource.Registration[sensor.Sensor, *CPUConfig]{
			Constructor: newWindowsDiagnosticsCPU,
		},
	)
}

type CPUConfig struct{}

func (cfg *CPUConfig) Validate(path string) ([]string, []string, error) {
	return nil, nil, nil
}

type windowsDiagnosticsCPU struct {
	resource.AlwaysRebuild
	resource.Named

	logger logging.Logger

	mu        sync.Mutex
	prevIdle  uint64
	prevTotal uint64
	hasPrev   bool

	cancelCtx  context.Context
	cancelFunc func()
}

func newWindowsDiagnosticsCPU(
	ctx context.Context,
	deps resource.Dependencies,
	rawConf resource.Config,
	logger logging.Logger,
) (sensor.Sensor, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())
	return &windowsDiagnosticsCPU{
		Named:      rawConf.ResourceName().AsNamed(),
		logger:     logger,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
	}, nil
}

// Readings returns CPU usage computed as the delta between successive calls to
// GetSystemTimes. The first call always returns 0% used because there is no
// previous sample to diff against.
func (s *windowsDiagnosticsCPU) Readings(
	ctx context.Context,
	extra map[string]interface{},
) (map[string]interface{}, error) {
	s.logger.Debug("CPU Readings called")

	var idleTime, kernelTime, userTime windows.Filetime
	if err := getSystemTimes(&idleTime, &kernelTime, &userTime); err != nil {
		return nil, err
	}

	idle := filetimeToUint64(idleTime)
	// kernelTime includes idleTime, so total = kernel + user avoids double-counting
	total := filetimeToUint64(kernelTime) + filetimeToUint64(userTime)

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.hasPrev {
		s.prevIdle = idle
		s.prevTotal = total
		s.hasPrev = true
		return map[string]interface{}{
			"used_percent": 0.0,
			"idle_percent": 100.0,
		}, nil
	}

	deltaIdle := idle - s.prevIdle
	deltaTotal := total - s.prevTotal
	s.prevIdle = idle
	s.prevTotal = total

	usedPercent := 0.0
	idlePercent := 100.0
	if deltaTotal > 0 {
		idlePercent = math.Round(float64(deltaIdle)/float64(deltaTotal)*100*10) / 10
		usedPercent = math.Round((100.0-idlePercent)*10) / 10
	}

	return map[string]interface{}{
		"used_percent": usedPercent,
		"idle_percent": idlePercent,
	}, nil
}

func (s *windowsDiagnosticsCPU) DoCommand(
	ctx context.Context,
	cmd map[string]interface{},
) (map[string]interface{}, error) {
	return nil, errUnimplemented
}

func (s *windowsDiagnosticsCPU) Close(context.Context) error {
	s.cancelFunc()
	return nil
}

func filetimeToUint64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}
