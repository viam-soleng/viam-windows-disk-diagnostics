package main

import (
	"windowsdiagnostics/components"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
)

func main() {
	module.ModularMain(
		resource.APIModel{sensor.API, components.Disk},
		resource.APIModel{sensor.API, components.CPU},
		resource.APIModel{sensor.API, components.Memory},
	)
}
