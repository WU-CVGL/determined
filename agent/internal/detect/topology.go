package detect

import (
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

const (
	// nvmlTimeout bounds the whole NVML session: loading the library, Init, the driver query and
	// the measurement. Device detection before it runs nvidia-smi without a timeout.
	nvmlTimeout = 60 * time.Second

	// NVMLInit values without an NVML return. Their codes are negative, unlike NVML's.
	nvmlInitNotBuilt     = "NOT_BUILT" // the agent was built without NVML support
	nvmlInitNotBuiltCode = -1
	nvmlInitTimeout      = "TIMEOUT" // the session did not finish within nvmlTimeout
	nvmlInitTimeoutCode  = -2

	reasonMIG = "MIG instances: GPU topology not collected"
	// reasonStillRunning: a session that did not finish within its deadline may still be blocked
	// in NVML, and a second Init next to it could block too.
	reasonStillRunning = "an earlier NVML session has not finished"
	migUUIDPrefix      = "MIG-"
)

// GPUCollection is the result of one NVML session: one Init, one driver query and the
// measurement of the inventory.
type GPUCollection struct {
	// NVMLInit is NVML's return from Init by symbolic name, for example "SUCCESS" or
	// "ERROR_LIBRARY_NOT_FOUND". It is "NOT_BUILT" for an agent built without NVML support, and
	// "TIMEOUT" when the session did not finish within its deadline; Init's own return is then
	// not known.
	NVMLInit string
	// NVMLInitCode is NVML's return code from Init: -1 for NOT_BUILT, -2 for TIMEOUT.
	NVMLInitCode int
	// DriverVersion is set when Init succeeded and NVML reported it.
	DriverVersion string
	// Topology is the agent's report. It is nil only when there is neither a CUDA slot nor an
	// excluded GPU. Whatever fails, it lists every slot and excluded GPU; UnknownReason then says
	// why they were not measured, and a GPU's NVMLError lists its failed calls.
	Topology *aproto.GPUTopology
}

// nvmlSession runs one NVML session over an inventory, which it owns. With an empty inventory it
// only initializes NVML and reads the driver version. runNVMLSession is the real one.
type nvmlSession func(inventory []aproto.GPUInfo) GPUCollection

// nvmlRunning is held while an NVML session runs. A cgo call cannot be canceled, so a session
// that missed its deadline may still be blocked in NVML; a later collection then reports a
// timeout instead of starting a second Init next to it.
var nvmlRunning = make(chan struct{}, 1)

// DetectGPUTopology is the agent's measurement at start: the topology, P2P status, PCIe links and
// NVML errors of its CUDA slots (devices) and of the GPUs its exclude list left out (excluded).
// It never fails and waits at most a minute for NVML: whatever goes wrong, it returns the
// inventory with an UnknownReason. It returns nil, without loading NVML, when there is neither a
// CUDA slot nor an excluded GPU.
func DetectGPUTopology(devices, excluded []device.Device) *aproto.GPUTopology {
	topo := collectGPUs(devices, excluded, false, runNVMLSession, nvmlTimeout).Topology
	logGPUTopology(topo)
	return topo
}

// CollectGPUs is the measurement of DetectGPUTopology for the gpu-topology subcommand. It also
// loads NVML when there is no GPU to measure, so that NVMLInit tells whether the library loads.
func CollectGPUs(devices, excluded []device.Device) GPUCollection {
	return collectGPUs(devices, excluded, true, runNVMLSession, nvmlTimeout)
}

// collectGPUs runs at most one session: probe runs it also without GPUs to measure.
func collectGPUs(
	devices, excluded []device.Device, probe bool, session nvmlSession, timeout time.Duration,
) GPUCollection {
	inventory := gpuInventory(devices, excluded)
	for _, g := range inventory {
		if strings.HasPrefix(g.UUID, migUUIDPrefix) {
			var c GPUCollection
			if probe {
				c = runSession(session, nil, timeout)
			}
			c.Topology = unmeasured(inventory, reasonMIG)
			return c
		}
	}
	if inventory == nil && !probe {
		return GPUCollection{}
	}
	return runSession(session, inventory, timeout)
}

// runSession runs the session on its own goroutine, with its own copy of the inventory, and waits
// at most timeout. On a timeout the session may go on running; its result is discarded.
func runSession(session nvmlSession, inventory []aproto.GPUInfo, timeout time.Duration) GPUCollection {
	timedOut := func(reason string) GPUCollection {
		return GPUCollection{
			NVMLInit:     nvmlInitTimeout,
			NVMLInitCode: nvmlInitTimeoutCode,
			Topology:     unmeasured(inventory, reason),
		}
	}
	select {
	case nvmlRunning <- struct{}{}:
	default:
		return timedOut(reasonStillRunning)
	}

	result := make(chan GPUCollection, 1)
	own := append([]aproto.GPUInfo(nil), inventory...)
	go func() {
		// The guard is released before the send: a caller that collects again as soon as it has
		// this result must find it free.
		c := func() GPUCollection {
			defer func() { <-nvmlRunning }()
			return session(own)
		}()
		result <- c
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case c := <-result:
		return c
	case <-timer.C:
		return timedOut(timeoutReason(timeout))
	}
}

func timeoutReason(timeout time.Duration) string {
	return "NVML did not finish within " + strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64) + "s"
}

// unmeasured is the report of an inventory that was not measured: its entries without
// telemetry. It is nil for an empty inventory.
func unmeasured(inventory []aproto.GPUInfo, reason string) *aproto.GPUTopology {
	if len(inventory) == 0 {
		return nil
	}
	gpus := make([]aproto.GPUInfo, 0, len(inventory))
	for _, g := range inventory {
		gpus = append(gpus, aproto.GPUInfo{UUID: g.UUID, Excluded: g.Excluded})
	}
	return &aproto.GPUTopology{UnknownReason: reason, GPUs: gpus}
}

// gpuInventory lists the GPUs to report, before any NVML call: one entry per CUDA slot, then
// one per excluded GPU. Collection fills their telemetry but never adds or drops an entry.
func gpuInventory(devices, excluded []device.Device) []aproto.GPUInfo {
	var inventory []aproto.GPUInfo
	for _, d := range devices {
		if d.Type == device.CUDA {
			inventory = append(inventory, aproto.GPUInfo{UUID: d.UUID})
		}
	}
	for _, d := range excluded {
		inventory = append(inventory, aproto.GPUInfo{UUID: d.UUID, Excluded: true})
	}
	return inventory
}

// logGPUTopology logs one summary line, and one line per failed NVML call of a GPU. The
// matrices, NUMA groups and P2P states are in the agent API, the CLI, the WebUI and the
// gpu-topology subcommand.
func logGPUTopology(topo *aproto.GPUTopology) {
	if topo == nil {
		return
	}
	slots, excluded, nvmlErrors := 0, 0, 0
	for _, g := range topo.GPUs {
		if g.Excluded {
			excluded++
		} else {
			slots++
		}
		if g.NVMLError != "" {
			nvmlErrors++
		}
	}
	switch {
	case topo.UnknownReason != "":
		log.Warnf("GPU topology unknown: slots=%d excluded=%d reason=%q",
			slots, excluded, topo.UnknownReason)
	case nvmlErrors > 0:
		log.Warnf("GPU topology collected: slots=%d excluded=%d nvml_errors=%d driver=%s",
			slots, excluded, nvmlErrors, topo.DriverVersion)
	default:
		log.Infof("GPU topology collected: slots=%d excluded=%d nvml_errors=0 driver=%s",
			slots, excluded, topo.DriverVersion)
	}
	for _, g := range topo.GPUs {
		if g.NVMLError == "" {
			continue
		}
		// NVMLError lists "<call>: <NAME> (<code>)" entries, separated by "; ".
		for _, e := range strings.Split(g.NVMLError, "; ") {
			call, ret, _ := strings.Cut(e, ": ")
			log.Warnf("GPU NVML error: uuid=%s excluded=%t call=%s return=%s", g.UUID, g.Excluded, call, ret)
		}
	}
}
