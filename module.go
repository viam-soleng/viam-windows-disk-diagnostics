//go:build windows

package windowsdiagnostics

import (
	"context"
	"errors"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"golang.org/x/sys/windows"
)

const defaultDiskPath = "C:\\"

var (
	Disk             = resource.NewModel("bill", "windows-diagnostics", "disk")
	errUnimplemented = errors.New("unimplemented")
)

func init() {
	resource.RegisterComponent(sensor.API, Disk,
		resource.Registration[sensor.Sensor, *Config]{
			Constructor: newWindowsDiagnosticsDisk,
		},
	)
}

type Config struct {
	Path string `json:"path"`
}

// Validate ensures all parts of the config are valid and important fields exist.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Path == "" {
		cfg.Path = defaultDiskPath
	}
	return nil, nil, nil
}

type windowsDiagnosticsDisk struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger
	cfg    *Config

	cancelCtx  context.Context
	cancelFunc func()
}

func newWindowsDiagnosticsDisk(
	ctx context.Context,
	deps resource.Dependencies,
	rawConf resource.Config,
	logger logging.Logger,
) (sensor.Sensor, error) {

	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	return NewDisk(ctx, deps, rawConf.ResourceName(), conf, logger)
}

func NewDisk(
	ctx context.Context,
	deps resource.Dependencies,
	name resource.Name,
	conf *Config,
	logger logging.Logger,
) (sensor.Sensor, error) {

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	s := &windowsDiagnosticsDisk{
		name:       name,
		logger:     logger,
		cfg:        conf,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
	}

	logger.Infof("Windows disk diagnostics using path %q", conf.Path)

	return s, nil
}

func (s *windowsDiagnosticsDisk) Name() resource.Name {
	return s.name
}

func (s *windowsDiagnosticsDisk) Readings(
	ctx context.Context,
	extra map[string]interface{},
) (map[string]interface{}, error) {

	path := normalizeDiskPath(s.cfg.Path)
	s.logger.Debugf("Resolved disk path: %q", path)

	total, free, available, err := getDiskUsage(path)
	if err != nil {
		return nil, err
	}

	used := total - free

	usedPercent := 0.0
	if total > 0 {
		usedPercent = float64(used) / float64(total) * 100
	}

	return map[string]interface{}{
		"path":            path,
		"total_bytes":     total,
		"free_bytes":      free,
		"available_bytes": available,
		"used_bytes":      used,
		"used_percent":    usedPercent,
	}, nil
}

func (s *windowsDiagnosticsDisk) DoCommand(
	ctx context.Context,
	cmd map[string]interface{},
) (map[string]interface{}, error) {
	return nil, errUnimplemented
}

func (s *windowsDiagnosticsDisk) Close(context.Context) error {
	s.cancelFunc()
	return nil
}

// --- helpers ---

func normalizeDiskPath(p string) string {
	// "C:" → "C:\"
	if len(p) == 2 && p[1] == ':' {
		return p + "\\"
	}
	// Defensive: "C" → "C:\"
	if len(p) == 1 {
		return p + ":\\"
	}
	return p
}

func getDiskUsage(path string) (total, free, available uint64, err error) {
	var freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes uint64

	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, 0, err
	}

	err = windows.GetDiskFreeSpaceEx(
		p,
		&freeBytesAvailable,
		&totalNumberOfBytes,
		&totalNumberOfFreeBytes,
	)
	if err != nil {
		return 0, 0, 0, err
	}

	return totalNumberOfBytes, totalNumberOfFreeBytes, freeBytesAvailable, nil
}
