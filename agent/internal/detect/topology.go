package detect

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

const (
	// gpuTopologyTimeout bounds how long agent start waits for the NVML collection (N5).
	gpuTopologyTimeout = 60 * time.Second

	reasonMIG     = "MIG instances: GPU topology not collected"
	migUUIDPrefix = "MIG-"
)

// NVMLInitStatus is the result of initializing NVML on its own (the diagnostic subcommand).
type NVMLInitStatus struct {
	// Name is the symbolic NVML return, for example "SUCCESS" or "ERROR_LIBRARY_NOT_FOUND", or
	// "NOT_BUILT" for an agent built without NVML support.
	Name string
	// Code is the NVML return code, or -1 for an agent built without NVML support.
	Code int
	// DriverVersion is set when Name is "SUCCESS" and NVML reports it.
	DriverVersion string
}

// topologyCollector measures the GPUs of an inventory. It owns the slice it is given.
type topologyCollector func(inventory []aproto.GPUInfo) *aproto.GPUTopology

// DetectGPUTopology measures the topology, P2P status, PCIe links and NVML errors of the agent's
// CUDA slots (devices) and of the GPUs its exclude list left out (excluded). It never fails and
// never blocks agent start for more than a minute: whatever goes wrong, it returns the inventory
// from device detection with an UnknownReason. It returns nil only when there is neither a CUDA
// device nor an excluded GPU.
func DetectGPUTopology(devices, excluded []device.Device) *aproto.GPUTopology {
	topo := detectGPUTopology(devices, excluded, collectGPUTopology, gpuTopologyTimeout)
	logGPUTopology(topo, devices)
	return topo
}

func detectGPUTopology(
	devices, excluded []device.Device, collector topologyCollector, timeout time.Duration,
) *aproto.GPUTopology {
	inventory := gpuInventory(devices, excluded)
	if inventory == nil {
		return nil
	}
	for _, g := range inventory {
		if strings.HasPrefix(g.UUID, migUUIDPrefix) {
			return &aproto.GPUTopology{UnknownReason: reasonMIG, GPUs: inventory}
		}
	}

	// The collector runs on its own copy: on a timeout it may still be running (a cgo call cannot
	// be canceled), and its result is discarded.
	result := make(chan *aproto.GPUTopology, 1)
	own := append([]aproto.GPUInfo(nil), inventory...)
	go func() { result <- collector(own) }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case topo := <-result:
		return topo
	case <-timer.C:
		return &aproto.GPUTopology{
			UnknownReason: timeoutReason(timeout),
			GPUs:          inventory,
		}
	}
}

func timeoutReason(timeout time.Duration) string {
	return "NVML collection did not finish within " +
		strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64) + "s"
}

// gpuInventory lists the GPUs to report, before any NVML call (N6): one entry per CUDA slot, then
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

// logGPUTopology logs one line: Info when the topology is known and no GPU has an NVML error,
// Warn otherwise.
func logGPUTopology(topo *aproto.GPUTopology, devices []device.Device) {
	if topo == nil {
		return
	}
	if topo.UnknownReason != "" {
		log.Warnf("GPU topology unknown: %s (%d GPUs)", topo.UnknownReason, len(topo.GPUs))
		return
	}
	line, hasErrors := gpuTopologySummary(topo, devices)
	if hasErrors {
		log.Warn(line)
		return
	}
	log.Info(line)
}

// gpuTopologySummary renders the log line, for example "GPU topology: 8 GPUs, NUMA 4+4, levels
// NODE/SYS, P2P usable, width below max at start: slot 1 (x8 of x16), NVML errors: none,
// driver 610.57.04".
func gpuTopologySummary(topo *aproto.GPUTopology, devices []device.Device) (string, bool) {
	slotOf := map[string]string{}
	for _, d := range devices {
		if d.Type == device.CUDA {
			slotOf[d.UUID] = "slot " + strconv.Itoa(int(d.ID))
		}
	}
	name := func(g aproto.GPUInfo) string {
		if s, ok := slotOf[g.UUID]; ok && !g.Excluded {
			return s
		}
		if g.PCIBusID != "" {
			return "excluded " + g.PCIBusID
		}
		return "excluded " + g.UUID
	}

	slots := map[string]bool{}
	numa := map[int]int{} // NUMA node (-1 unknown) to slot count
	var narrow, nvmlErrors []string
	numExcluded := 0
	for _, g := range topo.GPUs {
		if g.Excluded {
			numExcluded++
		} else {
			slots[g.UUID] = true
			key := -1
			if g.NUMANode != nil {
				key = *g.NUMANode
			}
			numa[key]++
		}
		if g.PCIeLinkWidth > 0 && g.PCIeLinkWidthMax > 0 && g.PCIeLinkWidth < g.PCIeLinkWidthMax {
			narrow = append(narrow, fmt.Sprintf("%s (x%d of x%d)", name(g), g.PCIeLinkWidth,
				g.PCIeLinkWidthMax))
		}
		if g.NVMLError != "" {
			nvmlErrors = append(nvmlErrors, fmt.Sprintf("%s (%s)", name(g), g.NVMLError))
		}
	}
	numaNodes := make([]int, 0, len(numa))
	for k := range numa {
		numaNodes = append(numaNodes, k)
	}
	sort.Ints(numaNodes)
	numaSizes := make([]string, 0, len(numaNodes))
	for _, k := range numaNodes {
		if k >= 0 {
			numaSizes = append(numaSizes, strconv.Itoa(numa[k]))
		}
	}
	if n := numa[-1]; n > 0 {
		numaSizes = append(numaSizes, strconv.Itoa(n)+" unknown")
	}

	levelSeen := map[aproto.GPULinkLevel]bool{}
	usable, notUsable, unknown := 0, 0, 0
	for _, l := range topo.Links {
		if !slots[l.UUIDA] || !slots[l.UUIDB] {
			continue
		}
		if l.Level.Known() {
			levelSeen[l.Level] = true
		}
		switch aproto.P2PUsability(l) {
		case aproto.GPUP2PUsable:
			usable++
		case aproto.GPUP2PNotUsable:
			notUsable++
		default:
			unknown++
		}
	}
	var levels []string
	for _, l := range []aproto.GPULinkLevel{
		aproto.GPULinkLevelInternal, aproto.GPULinkLevelPIX, aproto.GPULinkLevelPXB,
		aproto.GPULinkLevelPHB, aproto.GPULinkLevelNode, aproto.GPULinkLevelSys,
	} {
		if levelSeen[l] {
			levels = append(levels, string(l))
		}
	}
	if len(levels) == 0 {
		levels = []string{"unknown"}
	}

	total := usable + notUsable + unknown
	var p2p string
	switch {
	case total == 0:
		p2p = "P2P n/a"
	case usable == total:
		p2p = "P2P usable"
	case unknown == total:
		p2p = "P2P unknown"
	case usable == 0 && unknown == 0:
		p2p = "P2P not usable"
	default:
		p2p = fmt.Sprintf("P2P usable %d/%d", usable, total)
		if unknown > 0 {
			p2p += fmt.Sprintf(" (%d unknown)", unknown)
		}
	}

	gpus := fmt.Sprintf("%d GPUs", len(topo.GPUs))
	if numExcluded > 0 {
		gpus += fmt.Sprintf(" (%d excluded)", numExcluded)
	}
	parts := []string{
		gpus,
		"NUMA " + strings.Join(numaSizes, "+"),
		"levels " + strings.Join(levels, "/"),
		p2p,
		"width below max at start: " + noneIfEmpty(narrow),
		"NVML errors: " + noneIfEmpty(nvmlErrors),
		"driver " + topo.DriverVersion,
	}
	return "GPU topology: " + strings.Join(parts, ", "), len(nvmlErrors) > 0
}

func noneIfEmpty(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}
