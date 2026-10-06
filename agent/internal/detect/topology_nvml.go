//go:build linux && cgo

package detect

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	log "github.com/sirupsen/logrus"

	"github.com/determined-ai/determined/master/pkg/aproto"
)

const (
	sysfsPCIDevices = "/sys/bus/pci/devices"
	// sysfsNodesOnline lists the online NUMA nodes, for example "0" or "0-1".
	sysfsNodesOnline = "/sys/devices/system/node/online"
)

// runNVMLSession measures the inventory with the NVML library that the NVIDIA container toolkit
// mounts into the agent container (libnvidia-ml.so.1).
func runNVMLSession(inventory []aproto.GPUInfo) GPUCollection {
	return collect(nvml.New(), inventory, numaNodeReader(sysfsPCIDevices, sysfsNodesOnline), time.Now)
}

func shutdownNVML(lib nvml.Interface) {
	if ret := lib.Shutdown(); ret != nvml.SUCCESS {
		log.Debugf("NVML shutdown: %s", nvmlReturnString(ret))
	}
}

// collect is one NVML session: Init, the driver version, the measurement of the inventory and
// Shutdown. With an empty inventory it stops after the driver version. It fills the telemetry of
// each inventory entry as far as its NVML calls succeed, and measures every pair of them. It
// queries exactly the inventory's UUIDs, never another GPU, and never adds or drops an entry.
func collect(
	lib nvml.Interface,
	inventory []aproto.GPUInfo,
	numa func(bdf string) *int,
	now func() time.Time,
) (c GPUCollection) {
	ret := lib.Init()
	c.NVMLInit, c.NVMLInitCode = nvmlReturnName(ret), int(ret)
	if ret != nvml.SUCCESS {
		c.Topology = unmeasured(inventory, "NVML init: "+nvmlReturnString(ret))
		return c
	}
	defer shutdownNVML(lib)
	if v, ret := lib.SystemGetDriverVersion(); ret == nvml.SUCCESS {
		c.DriverVersion = v
	}
	if len(inventory) == 0 {
		return c
	}
	defer func() {
		// A Go panic in the measurement never fails agent start; a crash inside the C library
		// still does.
		if r := recover(); r != nil {
			log.Errorf("GPU topology collection panicked: %v", r)
			c.Topology = unmeasured(inventory, "NVML collection failed")
		}
	}()

	collectedAt := now()
	topo := &aproto.GPUTopology{
		CollectedAt: &collectedAt, DriverVersion: c.DriverVersion, GPUs: inventory,
	}
	handles := make([]nvml.Device, len(topo.GPUs))
	for i := range topo.GPUs {
		handles[i] = collectGPU(lib, &topo.GPUs[i], numa)
	}

	nvlinks := countNVLinks(handles, topo.GPUs)
	for i := range topo.GPUs {
		for j := i + 1; j < len(topo.GPUs); j++ {
			if handles[i] == nil || handles[j] == nil {
				continue // A missing pair is unknown.
			}
			a, b := orderByUUID(topo.GPUs, i, j)
			topo.Links = append(topo.Links, collectLink(
				handles[a], handles[b], topo.GPUs[a].UUID, topo.GPUs[b].UUID,
				max(nvlinks[[2]int{a, b}], nvlinks[[2]int{b, a}]),
			))
		}
	}
	sort.Slice(topo.Links, func(i, j int) bool {
		if topo.Links[i].UUIDA != topo.Links[j].UUIDA {
			return topo.Links[i].UUIDA < topo.Links[j].UUIDA
		}
		return topo.Links[i].UUIDB < topo.Links[j].UUIDB
	})
	c.Topology = topo
	return c
}

// orderByUUID returns the indexes of two GPUs as (A, B) with UUIDA < UUIDB.
func orderByUUID(gpus []aproto.GPUInfo, i, j int) (a, b int) {
	if gpus[j].UUID < gpus[i].UUID {
		return j, i
	}
	return i, j
}

// collectGPU runs the health calls for one GPU and returns its handle, or nil if the handle lookup
// failed. SUCCESS uses the value; ERROR_NOT_SUPPORTED leaves the field unknown with no error, as
// the GPU simply lacks it; any other return leaves the field unknown and appends
// "<call>: <NAME> (<code>)" to NVMLError.
func collectGPU(lib nvml.Interface, g *aproto.GPUInfo, numa func(bdf string) *int) nvml.Device {
	var errs []string
	ok := func(call string, ret nvml.Return) bool {
		switch ret {
		case nvml.SUCCESS:
			return true
		case nvml.ERROR_NOT_SUPPORTED:
			return false
		default:
			errs = append(errs, call+": "+nvmlReturnString(ret))
			return false
		}
	}
	defer func() { g.NVMLError = strings.Join(errs, "; ") }()

	dev, ret := lib.DeviceGetHandleByUUID(g.UUID)
	if !ok("DeviceGetHandleByUUID", ret) {
		return nil
	}
	if pci, ret := dev.GetPciInfo(); ok("GetPciInfo", ret) {
		g.PCIBusID = normalizeBusID(busIDString(pci.BusId[:]))
		if g.PCIBusID != "" {
			g.NUMANode = numa(g.PCIBusID)
		}
	}
	if v, ret := dev.GetCurrPcieLinkWidth(); ok("GetCurrPcieLinkWidth", ret) {
		g.PCIeLinkWidth = v
	}
	if v, ret := dev.GetMaxPcieLinkWidth(); ok("GetMaxPcieLinkWidth", ret) {
		g.PCIeLinkWidthMax = v
	}
	if v, ret := dev.GetCurrPcieLinkGeneration(); ok("GetCurrPcieLinkGeneration", ret) {
		g.PCIeLinkGen = v
	}
	if v, ret := dev.GetMaxPcieLinkGeneration(); ok("GetMaxPcieLinkGeneration", ret) {
		g.PCIeLinkGenMax = v
	}
	return dev
}

// countNVLinks is the NVLink probe. It never sets an error: NVML answers only for the links a
// device has, and rejects the first one it lacks (ERROR_NOT_SUPPORTED for link 0 on a GPU without
// NVLink, ERROR_INVALID_ARGUMENT for link 4 on an RTX 3090), which would otherwise mark such GPUs
// red. So it stops at the first non-SUCCESS return. It counts the enabled links of GPU i whose
// remote end is GPU j, by ordered pair (i, j).
func countNVLinks(handles []nvml.Device, gpus []aproto.GPUInfo) map[[2]int]int {
	byBusID := map[string]int{}
	for i, g := range gpus {
		if handles[i] != nil && g.PCIBusID != "" {
			byBusID[g.PCIBusID] = i
		}
	}
	counts := map[[2]int]int{}
	for i, dev := range handles {
		if dev == nil {
			continue
		}
		for link := 0; link < nvml.NVLINK_MAX_LINKS; link++ {
			state, ret := dev.GetNvLinkState(link)
			if ret != nvml.SUCCESS {
				log.Debugf("NVLink probe of %s stopped at link %d: GetNvLinkState: %s",
					gpus[i].UUID, link, nvmlReturnString(ret))
				break
			}
			if state != nvml.FEATURE_ENABLED {
				continue
			}
			remote, ret := dev.GetNvLinkRemotePciInfo(link)
			if ret != nvml.SUCCESS {
				log.Debugf("NVLink probe of %s stopped at link %d: GetNvLinkRemotePciInfo: %s",
					gpus[i].UUID, link, nvmlReturnString(ret))
				break
			}
			j, ok := byBusID[normalizeBusID(busIDString(remote.BusId[:]))]
			if ok && j != i {
				counts[[2]int{i, j}]++
			}
		}
	}
	return counts
}

// collectLink measures one pair (A, B). A pairwise failure makes only that value unknown. Values
// are used only on SUCCESS: TOPOLOGY_INTERNAL and P2P_STATUS_OK are both 0.
func collectLink(a, b nvml.Device, uuidA, uuidB string, nvlinks int) aproto.GPULink {
	link := aproto.GPULink{UUIDA: uuidA, UUIDB: uuidB, NVLinks: nvlinks}
	if level, ret := a.GetTopologyCommonAncestor(b); ret == nvml.SUCCESS {
		link.Level = topologyLevel(level)
	} else {
		log.Debugf("GetTopologyCommonAncestor(%s, %s): %s", uuidA, uuidB, nvmlReturnString(ret))
	}
	link.P2PAToB = p2pCaps(a, b, uuidA, uuidB)
	link.P2PBToA = p2pCaps(b, a, uuidB, uuidA)
	return link
}

// p2pCaps queries READ and WRITE for one direction: P2P needs both OK in both directions, as NCCL
// requires.
func p2pCaps(from, to nvml.Device, uuidFrom, uuidTo string) aproto.GPUP2PCaps {
	query := func(index nvml.GpuP2PCapsIndex, name string) aproto.GPUP2PStatus {
		status, ret := from.GetP2PStatus(to, index)
		if ret != nvml.SUCCESS {
			log.Debugf("GetP2PStatus(%s, %s, %s): %s", uuidFrom, uuidTo, name, nvmlReturnString(ret))
			return ""
		}
		return p2pStatus(status)
	}
	return aproto.GPUP2PCaps{
		Read:  query(nvml.P2P_CAPS_INDEX_READ, "READ"),
		Write: query(nvml.P2P_CAPS_INDEX_WRITE, "WRITE"),
	}
}

// topologyLevel maps NVML's common-ancestor level by value; any other value is unknown.
func topologyLevel(level nvml.GpuTopologyLevel) aproto.GPULinkLevel {
	switch level {
	case nvml.TOPOLOGY_INTERNAL:
		return aproto.GPULinkLevelInternal
	case nvml.TOPOLOGY_SINGLE:
		return aproto.GPULinkLevelPIX
	case nvml.TOPOLOGY_MULTIPLE:
		return aproto.GPULinkLevelPXB
	case nvml.TOPOLOGY_HOSTBRIDGE:
		return aproto.GPULinkLevelPHB
	case nvml.TOPOLOGY_NODE:
		return aproto.GPULinkLevelNode
	case nvml.TOPOLOGY_SYSTEM:
		return aproto.GPULinkLevelSys
	default:
		return ""
	}
}

// p2pStatus maps an NVML P2P status by value. go-nvml spells value 1 two ways
// (P2P_STATUS_CHIPSET_NOT_SUPPORED and P2P_STATUS_CHIPSET_NOT_SUPPORTED), so the switch is on the
// integer. P2P_STATUS_UNKNOWN (6) and any other value are unknown.
func p2pStatus(status nvml.GpuP2PStatus) aproto.GPUP2PStatus {
	switch int32(status) {
	case 0:
		return aproto.GPUP2PStatusOK
	case 1:
		return aproto.GPUP2PStatusChipsetNotSupported
	case 2:
		return aproto.GPUP2PStatusGPUNotSupported
	case 3:
		return aproto.GPUP2PStatusTopologyNotSupported
	case 4:
		return aproto.GPUP2PStatusDisabledByRegkey
	case 5:
		return aproto.GPUP2PStatusNotSupported
	default:
		return ""
	}
}

// busIDString converts NVML's NUL-terminated bus id.
func busIDString(raw []int8) string {
	b := make([]byte, 0, len(raw))
	for _, c := range raw {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// normalizeBusID converts NVML's bus id to the sysfs form: "00000000:A1:00.0" becomes
// "0000:a1:00.0".
func normalizeBusID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	domain, rest, found := strings.Cut(s, ":")
	if !found {
		return s
	}
	d, err := strconv.ParseUint(domain, 16, 32)
	if err != nil {
		return s
	}
	return fmt.Sprintf("%04x:%s", d, rest)
}

// numaNodeReader reads a PCI device's NUMA node from sysfs (pciRoot/<bdf>/numa_node). The
// kernel reports -1 when the firmware assigns the device no node: on a host whose only online
// NUMA node is 0 (nodesOnline reads "0") that is node 0; otherwise it is unknown (nil), as is a
// read error, with no error. DeviceGetNumaNodeId is not used: the cluster's nodes do not
// support it.
func numaNodeReader(pciRoot, nodesOnline string) func(bdf string) *int {
	b, err := os.ReadFile(nodesOnline) // #nosec G304
	singleNode := err == nil && strings.TrimSpace(string(b)) == "0"
	return func(bdf string) *int {
		if bdf == "" || strings.ContainsAny(bdf, `/\`) {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(pciRoot, bdf, "numa_node")) // #nosec G304
		if err != nil {
			return nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		switch {
		case err != nil:
			return nil
		case n == -1 && singleNode:
			n = 0
		case n < 0:
			return nil
		}
		return &n
	}
}

// nvmlReturnNames is the symbolic name of every Return constant of go-nvml
// (pkg/nvml/const.go). go-nvml's built-in table lacks 27-29, and once the library is loaded,
// Return.String() and Error() return NVML's prose instead, which is not a stable identifier.
var nvmlReturnNames = map[nvml.Return]string{
	nvml.SUCCESS:                         "SUCCESS",
	nvml.ERROR_UNINITIALIZED:             "ERROR_UNINITIALIZED",
	nvml.ERROR_INVALID_ARGUMENT:          "ERROR_INVALID_ARGUMENT",
	nvml.ERROR_NOT_SUPPORTED:             "ERROR_NOT_SUPPORTED",
	nvml.ERROR_NO_PERMISSION:             "ERROR_NO_PERMISSION",
	nvml.ERROR_ALREADY_INITIALIZED:       "ERROR_ALREADY_INITIALIZED",
	nvml.ERROR_NOT_FOUND:                 "ERROR_NOT_FOUND",
	nvml.ERROR_INSUFFICIENT_SIZE:         "ERROR_INSUFFICIENT_SIZE",
	nvml.ERROR_INSUFFICIENT_POWER:        "ERROR_INSUFFICIENT_POWER",
	nvml.ERROR_DRIVER_NOT_LOADED:         "ERROR_DRIVER_NOT_LOADED",
	nvml.ERROR_TIMEOUT:                   "ERROR_TIMEOUT",
	nvml.ERROR_IRQ_ISSUE:                 "ERROR_IRQ_ISSUE",
	nvml.ERROR_LIBRARY_NOT_FOUND:         "ERROR_LIBRARY_NOT_FOUND",
	nvml.ERROR_FUNCTION_NOT_FOUND:        "ERROR_FUNCTION_NOT_FOUND",
	nvml.ERROR_CORRUPTED_INFOROM:         "ERROR_CORRUPTED_INFOROM",
	nvml.ERROR_GPU_IS_LOST:               "ERROR_GPU_IS_LOST",
	nvml.ERROR_RESET_REQUIRED:            "ERROR_RESET_REQUIRED",
	nvml.ERROR_OPERATING_SYSTEM:          "ERROR_OPERATING_SYSTEM",
	nvml.ERROR_LIB_RM_VERSION_MISMATCH:   "ERROR_LIB_RM_VERSION_MISMATCH",
	nvml.ERROR_IN_USE:                    "ERROR_IN_USE",
	nvml.ERROR_MEMORY:                    "ERROR_MEMORY",
	nvml.ERROR_NO_DATA:                   "ERROR_NO_DATA",
	nvml.ERROR_VGPU_ECC_NOT_SUPPORTED:    "ERROR_VGPU_ECC_NOT_SUPPORTED",
	nvml.ERROR_INSUFFICIENT_RESOURCES:    "ERROR_INSUFFICIENT_RESOURCES",
	nvml.ERROR_FREQ_NOT_SUPPORTED:        "ERROR_FREQ_NOT_SUPPORTED",
	nvml.ERROR_ARGUMENT_VERSION_MISMATCH: "ERROR_ARGUMENT_VERSION_MISMATCH",
	nvml.ERROR_DEPRECATED:                "ERROR_DEPRECATED",
	nvml.ERROR_NOT_READY:                 "ERROR_NOT_READY",
	nvml.ERROR_GPU_NOT_FOUND:             "ERROR_GPU_NOT_FOUND",
	nvml.ERROR_INVALID_STATE:             "ERROR_INVALID_STATE",
	nvml.ERROR_UNKNOWN:                   "ERROR_UNKNOWN",
}

// nvmlReturnName is the symbolic name of an NVML return, or UNKNOWN_RETURN. Never use
// Return.String(), Error() or %v for a Return.
func nvmlReturnName(ret nvml.Return) string {
	if name, ok := nvmlReturnNames[ret]; ok {
		return name
	}
	return "UNKNOWN_RETURN"
}

// nvmlReturnString formats an NVML return as name and code, for example
// "ERROR_GPU_IS_LOST (15)".
func nvmlReturnString(ret nvml.Return) string {
	return nvmlReturnName(ret) + " (" + strconv.Itoa(int(ret)) + ")"
}
