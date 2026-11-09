// Package userusage counts the resources that a user's allocations hold. It depends only on
// sproto and device, so that the resource managers' tests can check it against their own state.
package userusage

import (
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// Counts is what resources hold.
type Counts struct {
	GPUs, CPUSlots, Aux int
	// K8s is set when a resource is a Kubernetes job, which has no Aux containers.
	K8s bool
}

// Count counts what r holds; k8sPool and cpuPool describe r's resource pool. On agentrm each
// device counts by its own type, and a container without devices is one Aux container.
// Kubernetes devices carry no type, so they count by the pool's slot type, and Kubernetes has no
// Aux. A device of another type counts by the pool's slot type too.
func Count(r sproto.ResourcesSummary, k8sPool, cpuPool bool) Counts {
	n := 0
	for _, devices := range r.AgentDevices {
		n += len(devices)
	}
	var c Counts
	switch {
	case r.ResourcesType == sproto.ResourcesTypeK8sJob || k8sPool:
		c.K8s = true
		if cpuPool {
			c.CPUSlots = n
		} else {
			c.GPUs = n
		}
	case n == 0:
		c.Aux = 1
	default:
		for _, devices := range r.AgentDevices {
			for _, d := range devices {
				switch {
				case d.Type == device.CPU:
					c.CPUSlots++
				case d.Type == device.CUDA || d.Type == device.ROCM:
					c.GPUs++
				case cpuPool:
					c.CPUSlots++
				default:
					c.GPUs++
				}
			}
		}
	}
	return c
}
