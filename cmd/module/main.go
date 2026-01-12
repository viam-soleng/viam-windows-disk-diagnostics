package main

import (
	"windowsdiagnostics"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	sensor "go.viam.com/rdk/components/sensor"
)

func main() {
	// ModularMain can take multiple APIModel arguments, if your module implements multiple models.
	module.ModularMain(resource.APIModel{ sensor.API, windowsdiagnostics.Disk})
}
