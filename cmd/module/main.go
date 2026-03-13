package main

import (
	"windowsdiagnostics"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	sensor "go.viam.com/rdk/components/sensor"
)

func main() {
	module.ModularMain(
		resource.APIModel{sensor.API, windowsdiagnostics.Disk},
		resource.APIModel{sensor.API, windowsdiagnostics.CPU},
		resource.APIModel{sensor.API, windowsdiagnostics.Memory},
	)
}
