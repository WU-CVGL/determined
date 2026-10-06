//go:build !linux || !cgo

package detect

import "github.com/determined-ai/determined/master/pkg/aproto"

// reasonNotBuilt is the unknown reason of an agent built without NVML support (D21).
const reasonNotBuilt = "agent built without NVML support (needs linux and cgo)"

// runNVMLSession returns the inventory unmeasured: go-nvml needs cgo on Linux.
func runNVMLSession(inventory []aproto.GPUInfo) GPUCollection {
	return GPUCollection{
		NVMLInit:     nvmlInitNotBuilt,
		NVMLInitCode: nvmlInitNotBuiltCode,
		Topology:     unmeasured(inventory, reasonNotBuilt),
	}
}
