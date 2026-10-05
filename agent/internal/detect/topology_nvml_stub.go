//go:build !linux || !cgo

package detect

import "github.com/determined-ai/determined/master/pkg/aproto"

// reasonNotBuilt is the unknown reason of an agent built without NVML support (D21).
const reasonNotBuilt = "agent built without NVML support (needs linux and cgo)"

// collectGPUTopology returns the inventory unmeasured: go-nvml needs cgo on Linux.
func collectGPUTopology(inventory []aproto.GPUInfo) *aproto.GPUTopology {
	return &aproto.GPUTopology{UnknownReason: reasonNotBuilt, GPUs: inventory}
}

// ProbeNVMLInit reports that this agent was built without NVML support.
func ProbeNVMLInit() NVMLInitStatus {
	return NVMLInitStatus{Name: "NOT_BUILT", Code: -1}
}
