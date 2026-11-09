// Package userusage meters the resources that a user's allocations hold: what an allocation's
// resources are metered as, and its usage by day and by month. It is pure: the callers read the
// records and the resource managers' state.
package userusage

import (
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// Kind is what an allocation's resources are metered as.
type Kind string

const (
	// KindGPU is GPUs, metered per device.
	KindGPU Kind = "gpu"
	// KindCPU is CPU slots, metered per slot.
	KindCPU Kind = "cpu"
	// KindAux is an auxiliary container without slots, metered once per allocation.
	KindAux Kind = "aux"
	// KindNone is not metered.
	KindNone Kind = "none"
	// KindUnclassified is slots of an unknown type, metered per slot.
	KindUnclassified Kind = "unclassified"
)

// Assigned returns what the resources assigned to an allocation are metered as, and how many
// units; slots is the allocation's slot count, which counts for resources without devices.
//
// Agent containers count by their devices: any GPU device makes the allocation a GPU one with its
// GPU devices counted (CPU devices next to GPUs are not metered), otherwise CPU devices count as
// CPU slots, and an allocation without devices is one auxiliary container. Kubernetes devices
// carry the resource manager's slot type and follow the same rule, but a Kubernetes allocation
// without devices is not metered. Slurm jobs have no devices; their slots are unclassified.
func Assigned(rs []sproto.ResourcesSummary, slots int) (Kind, int) {
	if len(rs) == 0 {
		return KindNone, 0
	}
	var gpus, cpus, other int
	for _, r := range rs {
		for _, devices := range r.AgentDevices {
			for _, d := range devices {
				switch d.Type {
				case device.CUDA, device.ROCM:
					gpus++
				case device.CPU:
					cpus++
				default:
					other++
				}
			}
		}
	}
	switch {
	case gpus > 0:
		return KindGPU, gpus
	case cpus > 0:
		return KindCPU, cpus
	case other > 0:
		return KindUnclassified, other
	}
	switch rs[0].ResourcesType {
	case sproto.ResourcesTypeK8sJob:
		return KindNone, 0
	case sproto.ResourcesTypeSlurmJob:
		if slots > 0 {
			return KindUnclassified, slots
		}
		return KindNone, 0
	default:
		return KindAux, 1
	}
}

// LegacyKind returns what an allocation from before metering is metered as, from its slot count
// and its pool: without slots it is an auxiliary container, or nothing on Kubernetes; with slots
// it takes the pool's slot type. It returns false when the slot type does not tell.
func LegacyKind(slots int, k8s bool, slotType device.Type) (Kind, bool) {
	switch {
	case slots == 0 && k8s:
		return KindNone, true
	case slots == 0:
		return KindAux, true
	case slotType == device.CPU:
		return KindCPU, true
	case slotType == device.CUDA || slotType == device.ROCM:
		return KindGPU, true
	default:
		return "", false
	}
}
