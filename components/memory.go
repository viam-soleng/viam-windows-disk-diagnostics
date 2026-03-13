//go:build windows

package components

import (
	"context"
	"math"
	"unsafe"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"golang.org/x/sys/windows"
)

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

func globalMemoryStatusEx(ms *memoryStatusEx) error {
	r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(ms)))
	if r == 0 {
		return e
	}
	return nil
}

var Memory = resource.NewModel("viam", "windows-diagnostics", "memory")

func init() {
	resource.RegisterComponent(sensor.API, Memory,
		resource.Registration[sensor.Sensor, *MemoryConfig]{
			Constructor: newWindowsDiagnosticsMemory,
		},
	)
}

type MemoryConfig struct{}

func (cfg *MemoryConfig) Validate(path string) ([]string, []string, error) {
	return nil, nil, nil
}

type windowsDiagnosticsMemory struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger

	cancelCtx  context.Context
	cancelFunc func()
}

func newWindowsDiagnosticsMemory(
	ctx context.Context,
	deps resource.Dependencies,
	rawConf resource.Config,
	logger logging.Logger,
) (sensor.Sensor, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())
	return &windowsDiagnosticsMemory{
		name:       rawConf.ResourceName(),
		logger:     logger,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
	}, nil
}

func (s *windowsDiagnosticsMemory) Name() resource.Name { return s.name }

func (s *windowsDiagnosticsMemory) Readings(
	ctx context.Context,
	extra map[string]interface{},
) (map[string]interface{}, error) {
	s.logger.Debug("Memory Readings called")

	var memStatus memoryStatusEx
	memStatus.dwLength = uint32(unsafe.Sizeof(memStatus))
	if err := globalMemoryStatusEx(&memStatus); err != nil {
		return nil, err
	}

	total := memStatus.ullTotalPhys
	available := memStatus.ullAvailPhys
	used := total - available

	usedPercent := 0.0
	if total > 0 {
		usedPercent = math.Round(float64(used)/float64(total)*100*10) / 10
	}

	return map[string]interface{}{
		"total_bytes":     total,
		"available_bytes": available,
		"used_bytes":      used,
		"used_percent":    usedPercent,
	}, nil
}

func (s *windowsDiagnosticsMemory) DoCommand(
	ctx context.Context,
	cmd map[string]interface{},
) (map[string]interface{}, error) {
	return nil, errUnimplemented
}

func (s *windowsDiagnosticsMemory) Close(context.Context) error {
	s.cancelFunc()
	return nil
}
