//go:build linux && cgo

package detect

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// fakeGPU is one GPU of a fake NVML node.
type fakeGPU struct {
	uuid                         string
	busID                        string // NVML form, for example "00000000:41:00.0"
	width, widthMax, gen, genMax int
	// rets overrides the return of a health call by name, for example "GetCurrPcieLinkWidth".
	rets map[string]nvml.Return
	// nvlink answers GetNvLinkState and GetNvLinkRemotePciInfo for one link. nil means a GPU
	// without NVLink: ERROR_NOT_SUPPORTED for link 0.
	nvlink func(link int) (nvml.EnableState, nvml.Return, string, nvml.Return)
	// panics makes GetPciInfo panic, like a bug in the measurement.
	panics bool
}

type p2pCall struct {
	from, to int
	index    nvml.GpuP2PCapsIndex
}

// fakeNode is a fake NVML library over a set of GPUs.
type fakeNode struct {
	t       *testing.T
	gpus    []*fakeGPU
	initRet nvml.Return
	// initBlock, when set, blocks Init until it is closed: an NVML that hangs.
	initBlock chan struct{}
	driver    string
	// level and p2p answer the pairwise calls by GPU index; nil gives NODE within a group of four
	// and SYS across, and P2P OK.
	level func(a, b int) (nvml.GpuTopologyLevel, nvml.Return)
	p2p   func(from, to int, index nvml.GpuP2PCapsIndex) (nvml.GpuP2PStatus, nvml.Return)

	mu             sync.Mutex
	handleRequests []string
	p2pCalls       []p2pCall
	nvlinkCalls    map[int][]int
	inits          int
	shutdowns      int
}

func pciInfo(busID string) nvml.PciInfo {
	var info nvml.PciInfo
	for i := 0; i < len(busID) && i < len(info.BusId)-1; i++ {
		info.BusId[i] = int8(busID[i])
	}
	return info
}

func (f *fakeNode) lib() *mock.Interface {
	devs := make([]*mock.Device, len(f.gpus))
	index := map[nvml.Device]int{}
	f.nvlinkCalls = map[int][]int{}
	for i, g := range f.gpus {
		ret := func(call string) nvml.Return {
			if r, ok := g.rets[call]; ok {
				return r
			}
			return nvml.SUCCESS
		}
		devs[i] = &mock.Device{
			GetPciInfoFunc: func() (nvml.PciInfo, nvml.Return) {
				if g.panics {
					panic("index out of range")
				}
				return pciInfo(g.busID), ret("GetPciInfo")
			},
			GetCurrPcieLinkWidthFunc: func() (int, nvml.Return) {
				return g.width, ret("GetCurrPcieLinkWidth")
			},
			GetMaxPcieLinkWidthFunc: func() (int, nvml.Return) {
				return g.widthMax, ret("GetMaxPcieLinkWidth")
			},
			GetCurrPcieLinkGenerationFunc: func() (int, nvml.Return) {
				return g.gen, ret("GetCurrPcieLinkGeneration")
			},
			GetMaxPcieLinkGenerationFunc: func() (int, nvml.Return) {
				return g.genMax, ret("GetMaxPcieLinkGeneration")
			},
			GetNvLinkStateFunc: func(link int) (nvml.EnableState, nvml.Return) {
				f.mu.Lock()
				f.nvlinkCalls[i] = append(f.nvlinkCalls[i], link)
				f.mu.Unlock()
				if g.nvlink == nil {
					return nvml.FEATURE_DISABLED, nvml.ERROR_NOT_SUPPORTED
				}
				state, r, _, _ := g.nvlink(link)
				return state, r
			},
			GetNvLinkRemotePciInfoFunc: func(link int) (nvml.PciInfo, nvml.Return) {
				require.NotNil(f.t, g.nvlink, "remote PCI info of a GPU without NVLink")
				state, _, remote, r := g.nvlink(link)
				// N2 queries the remote end only of an enabled link. Non-fatal: the collector may
				// run on its own goroutine.
				if state != nvml.FEATURE_ENABLED {
					f.t.Errorf("remote PCI info of GPU %s link %d, which is not FEATURE_ENABLED",
						g.uuid, link)
				}
				return pciInfo(remote), r
			},
			GetTopologyCommonAncestorFunc: func(other nvml.Device) (nvml.GpuTopologyLevel, nvml.Return) {
				j := index[other]
				if f.level != nil {
					return f.level(i, j)
				}
				if i/4 == j/4 {
					return nvml.TOPOLOGY_NODE, nvml.SUCCESS
				}
				return nvml.TOPOLOGY_SYSTEM, nvml.SUCCESS
			},
			GetP2PStatusFunc: func(
				other nvml.Device, idx nvml.GpuP2PCapsIndex,
			) (nvml.GpuP2PStatus, nvml.Return) {
				j := index[other]
				f.mu.Lock()
				f.p2pCalls = append(f.p2pCalls, p2pCall{from: i, to: j, index: idx})
				f.mu.Unlock()
				if f.p2p != nil {
					return f.p2p(i, j, idx)
				}
				return nvml.P2P_STATUS_OK, nvml.SUCCESS
			},
		}
		index[devs[i]] = i
	}
	byUUID := map[string]int{}
	for i, g := range f.gpus {
		byUUID[g.uuid] = i
	}
	return &mock.Interface{
		InitFunc: func() nvml.Return {
			f.mu.Lock()
			f.inits++
			f.mu.Unlock()
			if f.initBlock != nil {
				<-f.initBlock
			}
			return f.initRet
		},
		ShutdownFunc: func() nvml.Return {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.shutdowns++
			return nvml.SUCCESS
		},
		SystemGetDriverVersionFunc: func() (string, nvml.Return) {
			if f.driver == "" {
				return "", nvml.ERROR_NOT_SUPPORTED
			}
			return f.driver, nvml.SUCCESS
		},
		DeviceGetHandleByUUIDFunc: func(uuid string) (nvml.Device, nvml.Return) {
			f.mu.Lock()
			f.handleRequests = append(f.handleRequests, uuid)
			f.mu.Unlock()
			i, ok := byUUID[uuid]
			if !ok {
				return nil, nvml.ERROR_NOT_FOUND
			}
			if r, ok := f.gpus[i].rets["DeviceGetHandleByUUID"]; ok && r != nvml.SUCCESS {
				return nil, r
			}
			return devs[i], nvml.SUCCESS
		},
	}
}

// eightGPUs returns a node07-like set: 8 GPUs, x16 Gen4, bus ids 0x01..0x08 on two sockets.
func eightGPUs() []*fakeGPU {
	gpus := make([]*fakeGPU, 8)
	for i := range gpus {
		gpus[i] = &fakeGPU{
			uuid:  fmt.Sprintf("GPU-%d0000000-0000-0000-0000-000000000000", i),
			busID: fmt.Sprintf("00000000:%02X:00.0", 0x41+i*0x10),
			width: 16, widthMax: 16, gen: 4, genMax: 4,
		}
	}
	return gpus
}

func inventoryOf(gpus []*fakeGPU, excluded ...int) []aproto.GPUInfo {
	isExcluded := map[int]bool{}
	for _, i := range excluded {
		isExcluded[i] = true
	}
	var inv []aproto.GPUInfo
	for i, g := range gpus {
		inv = append(inv, aproto.GPUInfo{UUID: g.uuid, Excluded: isExcluded[i]})
	}
	return inv
}

// numaByBus is the injected NUMA reader: socket 0 for bus ids below 0x80.
func numaByBus(bdf string) *int {
	var bus int
	if _, err := fmt.Sscanf(bdf, "0000:%x:", &bus); err != nil {
		return nil
	}
	n := bus / 0x80
	return &n
}

var fixedNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// session is the NVML session over the fake library.
func (f *fakeNode) session() nvmlSession {
	return func(inv []aproto.GPUInfo) GPUCollection {
		return collect(f.lib(), inv, numaByBus, func() time.Time { return fixedNow })
	}
}

func (f *fakeNode) counts() (inits, shutdowns int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inits, f.shutdowns
}

func collectFake(f *fakeNode, inv []aproto.GPUInfo) *aproto.GPUTopology {
	return f.session()(inv).Topology
}

// slotsAndExcluded splits a fake node's GPUs into CUDA slots and the excluded GPUs, by index.
func slotsAndExcluded(gpus []*fakeGPU, excluded ...int) (devices, excl []device.Device) {
	isExcluded := map[int]bool{}
	for _, i := range excluded {
		isExcluded[i] = true
	}
	for i, g := range gpus {
		d := device.Device{ID: device.ID(i), UUID: g.uuid, Type: device.CUDA}
		if isExcluded[i] {
			excl = append(excl, d)
		} else {
			devices = append(devices, d)
		}
	}
	return devices, excl
}

func findLink(t *testing.T, topo *aproto.GPUTopology, u1, u2 string) aproto.GPULink {
	t.Helper()
	for _, l := range topo.Links {
		if (l.UUIDA == u1 && l.UUIDB == u2) || (l.UUIDA == u2 && l.UUIDB == u1) {
			return l
		}
	}
	require.Failf(t, "link not found", "%s %s", u1, u2)
	return aproto.GPULink{}
}

func TestCollectNode07Like(t *testing.T) {
	gpus := eightGPUs()
	gpus[1].width, gpus[1].gen = 8, 1
	f := &fakeNode{t: t, gpus: gpus, driver: "610.57.04"}
	topo := collectFake(f, inventoryOf(gpus))

	require.Empty(t, topo.UnknownReason)
	require.NotNil(t, topo.CollectedAt)
	require.Equal(t, fixedNow, *topo.CollectedAt)
	require.Equal(t, "610.57.04", topo.DriverVersion)
	require.Len(t, topo.GPUs, 8)
	for i, g := range topo.GPUs {
		require.Equal(t, gpus[i].uuid, g.UUID)
		require.Equal(t, fmt.Sprintf("0000:%02x:00.0", 0x41+i*0x10), g.PCIBusID)
		require.NotNil(t, g.NUMANode)
		require.Equal(t, (0x41+i*0x10)/0x80, *g.NUMANode)
		require.Empty(t, g.NVMLError)
		require.False(t, g.Excluded)
		require.Equal(t, 16, g.PCIeLinkWidthMax)
		require.Equal(t, 4, g.PCIeLinkGenMax)
	}
	require.Equal(t, 8, topo.GPUs[1].PCIeLinkWidth)
	require.Equal(t, 1, topo.GPUs[1].PCIeLinkGen)
	require.Equal(t, 16, topo.GPUs[0].PCIeLinkWidth)

	require.Len(t, topo.Links, 28)
	for _, l := range topo.Links {
		require.Less(t, l.UUIDA, l.UUIDB)
		require.Equal(t, aproto.GPUP2PUsable, aproto.P2PUsability(l))
		require.Zero(t, l.NVLinks)
	}
	require.Equal(t, aproto.GPULinkLevelNode, findLink(t, topo, gpus[0].uuid, gpus[3].uuid).Level)
	require.Equal(t, aproto.GPULinkLevelSys, findLink(t, topo, gpus[3].uuid, gpus[4].uuid).Level)
	require.Equal(t, 1, f.shutdowns)
}

func TestCollectReturnCodeRule(t *testing.T) {
	gpus := eightGPUs()
	gpus[3].rets = map[string]nvml.Return{"GetCurrPcieLinkWidth": nvml.ERROR_GPU_IS_LOST}
	gpus[5].rets = map[string]nvml.Return{"GetMaxPcieLinkGeneration": nvml.ERROR_NOT_SUPPORTED}
	f := &fakeNode{
		t: t, gpus: gpus,
		// The zero-value trap: a failed call returns 0, which is TOPOLOGY_INTERNAL and
		// P2P_STATUS_OK.
		level: func(a, b int) (nvml.GpuTopologyLevel, nvml.Return) {
			if a+b == 1 { // the pair (0, 1)
				return nvml.TOPOLOGY_INTERNAL, nvml.ERROR_UNKNOWN
			}
			return nvml.TOPOLOGY_NODE, nvml.SUCCESS
		},
		p2p: func(from, to int, idx nvml.GpuP2PCapsIndex) (nvml.GpuP2PStatus, nvml.Return) {
			if from == 2 && to == 6 && idx == nvml.P2P_CAPS_INDEX_READ {
				return nvml.P2P_STATUS_OK, nvml.ERROR_GPU_IS_LOST
			}
			return nvml.P2P_STATUS_OK, nvml.SUCCESS
		},
	}
	topo := collectFake(f, inventoryOf(gpus))

	for i, g := range topo.GPUs {
		switch i {
		case 3:
			require.Equal(t, "GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)", g.NVMLError)
			require.Zero(t, g.PCIeLinkWidth)
			require.Equal(t, 16, g.PCIeLinkWidthMax)
		case 5:
			require.Empty(t, g.NVMLError, "NOT_SUPPORTED is unknown without an error")
			require.Zero(t, g.PCIeLinkGenMax)
			require.Equal(t, 4, g.PCIeLinkGen)
		default:
			require.Empty(t, g.NVMLError)
		}
	}

	l01 := findLink(t, topo, gpus[0].uuid, gpus[1].uuid)
	require.Empty(t, l01.Level, "a failed level query is unknown, not INTERNAL")
	require.Equal(t, aproto.GPUP2PUsable, aproto.P2PUsability(l01))

	l26 := findLink(t, topo, gpus[2].uuid, gpus[6].uuid)
	require.Equal(t, aproto.GPULinkLevelNode, l26.Level)
	require.Equal(t, gpus[2].uuid, l26.UUIDA)
	require.Empty(t, l26.P2PAToB.Read, "a failed P2P query is unknown, not OK")
	require.Equal(t, aproto.GPUP2PStatusOK, l26.P2PAToB.Write)
	require.Equal(t, aproto.GPUP2PUsability(""), aproto.P2PUsability(l26))
	for _, g := range []int{2, 6} {
		require.Empty(t, topo.GPUs[g].NVMLError, "a pairwise failure never sets a GPU's error")
	}
}

func TestCollectP2PReadWrite(t *testing.T) {
	gpus := eightGPUs()[:3]
	f := &fakeNode{
		t: t, gpus: gpus,
		p2p: func(from, to int, idx nvml.GpuP2PCapsIndex) (nvml.GpuP2PStatus, nvml.Return) {
			switch {
			case from == 1 && to == 0 && idx == nvml.P2P_CAPS_INDEX_WRITE:
				return nvml.P2P_STATUS_NOT_SUPPORTED, nvml.SUCCESS
			case from == 2 && to == 0 && idx == nvml.P2P_CAPS_INDEX_WRITE:
				return nvml.P2P_STATUS_OK, nvml.ERROR_UNKNOWN
			}
			return nvml.P2P_STATUS_OK, nvml.SUCCESS
		},
	}
	topo := collectFake(f, inventoryOf(gpus))

	// Four queries per unordered pair: READ and WRITE, both directions.
	require.Len(t, f.p2pCalls, 12)
	seen := map[p2pCall]int{}
	for _, c := range f.p2pCalls {
		seen[c]++
	}
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			if a == b {
				continue
			}
			for _, idx := range []nvml.GpuP2PCapsIndex{nvml.P2P_CAPS_INDEX_READ, nvml.P2P_CAPS_INDEX_WRITE} {
				require.Equal(t, 1, seen[p2pCall{from: a, to: b, index: idx}], "%d->%d %d", a, b, idx)
			}
		}
	}

	require.Equal(t, aproto.GPUP2PUsable, aproto.P2PUsability(findLink(t, topo, gpus[1].uuid, gpus[2].uuid)))

	l01 := findLink(t, topo, gpus[0].uuid, gpus[1].uuid)
	require.Equal(t, aproto.GPUP2PNotUsable, aproto.P2PUsability(l01))
	require.Equal(t, gpus[0].uuid, l01.UUIDA)
	require.Equal(t, aproto.GPUP2PCaps{Read: "OK", Write: "OK"}, l01.P2PAToB)
	require.Equal(t, aproto.GPUP2PCaps{Read: "OK", Write: "NOT_SUPPORTED"}, l01.P2PBToA)

	l02 := findLink(t, topo, gpus[0].uuid, gpus[2].uuid)
	require.Equal(t, aproto.GPUP2PCaps{Read: "OK", Write: ""}, l02.P2PBToA)
	require.Equal(t, aproto.GPUP2PUsability(""), aproto.P2PUsability(l02))
}

func TestCollectUUIDOrderSwapsDirections(t *testing.T) {
	gpus := eightGPUs()[:2]
	gpus[0].uuid, gpus[1].uuid = "GPU-b", "GPU-a" // detection order is not UUID order
	f := &fakeNode{
		t: t, gpus: gpus,
		p2p: func(from, to int, idx nvml.GpuP2PCapsIndex) (nvml.GpuP2PStatus, nvml.Return) {
			if from == 0 && idx == nvml.P2P_CAPS_INDEX_READ { // GPU-b -> GPU-a READ
				return nvml.P2P_STATUS_GPU_NOT_SUPPORTED, nvml.SUCCESS
			}
			return nvml.P2P_STATUS_OK, nvml.SUCCESS
		},
	}
	topo := collectFake(f, inventoryOf(gpus))
	require.Len(t, topo.Links, 1)
	l := topo.Links[0]
	require.Equal(t, "GPU-a", l.UUIDA)
	require.Equal(t, "GPU-b", l.UUIDB)
	require.Equal(t, aproto.GPUP2PCaps{Read: "OK", Write: "OK"}, l.P2PAToB)
	require.Equal(t, aproto.GPUP2PCaps{Read: "GPU_NOT_SUPPORTED", Write: "OK"}, l.P2PBToA)
}

func TestMapTopologyLevelAndP2PStatus(t *testing.T) {
	levels := []struct {
		in   nvml.GpuTopologyLevel
		want aproto.GPULinkLevel
	}{
		{nvml.TOPOLOGY_INTERNAL, "INTERNAL"},
		{nvml.TOPOLOGY_SINGLE, "PIX"},
		{nvml.TOPOLOGY_MULTIPLE, "PXB"},
		{nvml.TOPOLOGY_HOSTBRIDGE, "PHB"},
		{nvml.TOPOLOGY_NODE, "NODE"},
		{nvml.TOPOLOGY_SYSTEM, "SYS"},
		{5, ""},
		{60, ""},
		{-1, ""},
	}
	for _, c := range levels {
		require.Equal(t, c.want, topologyLevel(c.in), "level %d", c.in)
	}

	statuses := []struct {
		in   nvml.GpuP2PStatus
		want aproto.GPUP2PStatus
	}{
		{nvml.P2P_STATUS_OK, "OK"},
		{nvml.P2P_STATUS_CHIPSET_NOT_SUPPORED, "CHIPSET_NOT_SUPPORTED"},
		{nvml.P2P_STATUS_CHIPSET_NOT_SUPPORTED, "CHIPSET_NOT_SUPPORTED"},
		{nvml.P2P_STATUS_GPU_NOT_SUPPORTED, "GPU_NOT_SUPPORTED"},
		{nvml.P2P_STATUS_IOH_TOPOLOGY_NOT_SUPPORTED, "TOPOLOGY_NOT_SUPPORTED"},
		{nvml.P2P_STATUS_DISABLED_BY_REGKEY, "DISABLED_BY_REGKEY"},
		{nvml.P2P_STATUS_NOT_SUPPORTED, "NOT_SUPPORTED"},
		{nvml.P2P_STATUS_UNKNOWN, ""},
		{7, ""},
		{-1, ""},
	}
	for _, c := range statuses {
		require.Equal(t, c.want, p2pStatus(c.in), "status %d", c.in)
		if c.want != "" {
			require.True(t, c.want.Known())
		}
	}
}

func TestCollectOnlyDetectedUUIDs(t *testing.T) {
	// The node has 8 GPUs; visible_gpus (or a docker device list) left the agent 6 slots, and
	// GPU 7 is excluded. GPU 6 must never be queried.
	gpus := eightGPUs()
	f := &fakeNode{t: t, gpus: gpus}
	var devices []device.Device
	for i := 0; i < 6; i++ {
		devices = append(devices, device.Device{ID: device.ID(i), UUID: gpus[i].uuid, Type: device.CUDA})
	}
	excluded := []device.Device{{ID: 7, UUID: gpus[7].uuid, Type: device.CUDA}}

	topo := collectGPUs(devices, excluded, false, f.session(), time.Minute).Topology

	want := []string{gpus[0].uuid, gpus[1].uuid, gpus[2].uuid, gpus[3].uuid, gpus[4].uuid, gpus[5].uuid, gpus[7].uuid}
	got := append([]string(nil), f.handleRequests...)
	sort.Strings(got)
	require.Equal(t, want, got)
	require.Len(t, topo.GPUs, 7)
	require.Len(t, topo.Links, 21)
	for _, l := range topo.Links {
		require.NotEqual(t, gpus[6].uuid, l.UUIDA)
		require.NotEqual(t, gpus[6].uuid, l.UUIDB)
	}
}

func TestCollectExcludedGPUs(t *testing.T) {
	gpus := eightGPUs()
	gpus[4].rets = map[string]nvml.Return{"GetCurrPcieLinkWidth": nvml.ERROR_GPU_IS_LOST}
	f := &fakeNode{t: t, gpus: gpus}
	var devices []device.Device
	for _, i := range []int{0, 1, 2, 3, 5, 6, 7} {
		devices = append(devices, device.Device{ID: device.ID(i), UUID: gpus[i].uuid, Type: device.CUDA})
	}
	excluded := []device.Device{{ID: 4, UUID: gpus[4].uuid, Type: device.CUDA}}

	topo := collectGPUs(devices, excluded, false, f.session(), time.Minute).Topology

	require.Len(t, topo.GPUs, 8)
	for i, g := range topo.GPUs {
		if i == 7 {
			require.Equal(t, gpus[4].uuid, g.UUID, "excluded GPUs follow the slots")
			require.True(t, g.Excluded)
			require.Equal(t, "GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)", g.NVMLError)
			require.Equal(t, "0000:81:00.0", g.PCIBusID, "an excluded GPU gets the same calls")
			continue
		}
		require.False(t, g.Excluded)
		require.Empty(t, g.NVMLError, "an NVML error on an excluded GPU sets only its own")
	}
	require.Len(t, topo.Links, 28, "links with the excluded GPU are measured too")
}

func TestCollectAllExcludedKeepsTelemetry(t *testing.T) {
	gpus := eightGPUs()
	f := &fakeNode{t: t, gpus: gpus}
	var excluded []device.Device
	for i, g := range gpus {
		excluded = append(excluded, device.Device{ID: device.ID(i), UUID: g.uuid, Type: device.CUDA})
	}
	topo := collectGPUs(nil, excluded, false, f.session(), time.Minute).Topology
	require.NotNil(t, topo)
	require.Empty(t, topo.UnknownReason)
	require.Len(t, topo.GPUs, 8)
	for _, g := range topo.GPUs {
		require.True(t, g.Excluded)
		require.NotEmpty(t, g.PCIBusID)
		require.Equal(t, 16, g.PCIeLinkWidth)
	}
}

func TestCollectLibraryNotFound(t *testing.T) {
	gpus := eightGPUs()
	f := &fakeNode{t: t, gpus: gpus, initRet: nvml.ERROR_LIBRARY_NOT_FOUND}
	var devices []device.Device
	for i := 0; i < 7; i++ {
		devices = append(devices, device.Device{ID: device.ID(i), UUID: gpus[i].uuid, Type: device.CUDA})
	}
	excluded := []device.Device{{ID: 7, UUID: gpus[7].uuid, Type: device.CUDA}}
	topo := collectGPUs(devices, excluded, false, f.session(), time.Minute).Topology

	require.Equal(t, "NVML init: ERROR_LIBRARY_NOT_FOUND (12)", topo.UnknownReason)
	require.Equal(t, inventoryOf(gpus, 7), topo.GPUs, "the inventory, with no telemetry")
	require.Empty(t, topo.Links)
	require.Nil(t, topo.CollectedAt)
	require.Empty(t, f.handleRequests)
	require.Zero(t, f.shutdowns)
}

func TestCollectHandleLookupFails(t *testing.T) {
	gpus := eightGPUs()[:4]
	gpus[2].rets = map[string]nvml.Return{"DeviceGetHandleByUUID": nvml.ERROR_GPU_IS_LOST}
	f := &fakeNode{t: t, gpus: gpus}
	topo := collectFake(f, inventoryOf(gpus))

	require.Empty(t, topo.UnknownReason)
	require.Equal(t, aproto.GPUInfo{
		UUID: gpus[2].uuid, NVMLError: "DeviceGetHandleByUUID: ERROR_GPU_IS_LOST (15)",
	}, topo.GPUs[2])
	require.Len(t, topo.Links, 3, "pairs with GPU 2 are missing, so unknown")
	for _, l := range topo.Links {
		require.NotEqual(t, gpus[2].uuid, l.UUIDA)
		require.NotEqual(t, gpus[2].uuid, l.UUIDB)
	}
	require.NotEmpty(t, topo.GPUs[1].PCIBusID)
}

func TestCollectNVLinkCount(t *testing.T) {
	gpus := eightGPUs()[:3]
	// GPUs 0 and 1 share two NVLinks (links 0 and 1); link 2 of each goes to a bus that is not
	// ours (an NVSwitch, say); link 3 is disabled; GPU 2 has none.
	bridge := func(peer string) func(int) (nvml.EnableState, nvml.Return, string, nvml.Return) {
		return func(link int) (nvml.EnableState, nvml.Return, string, nvml.Return) {
			switch link {
			case 0, 1:
				return nvml.FEATURE_ENABLED, nvml.SUCCESS, peer, nvml.SUCCESS
			case 2:
				return nvml.FEATURE_ENABLED, nvml.SUCCESS, "00000000:FF:00.0", nvml.SUCCESS
			case 3:
				// A disabled link whose remote end is the peer: counting it would give 3.
				return nvml.FEATURE_DISABLED, nvml.SUCCESS, peer, nvml.SUCCESS
			default:
				return nvml.FEATURE_DISABLED, nvml.ERROR_INVALID_ARGUMENT, "", nvml.SUCCESS
			}
		}
	}
	gpus[0].nvlink = bridge(gpus[1].busID)
	gpus[1].nvlink = bridge(gpus[0].busID)
	f := &fakeNode{t: t, gpus: gpus}
	topo := collectFake(f, inventoryOf(gpus))

	require.Equal(t, 2, findLink(t, topo, gpus[0].uuid, gpus[1].uuid).NVLinks)
	require.Zero(t, findLink(t, topo, gpus[0].uuid, gpus[2].uuid).NVLinks)
	require.Zero(t, findLink(t, topo, gpus[1].uuid, gpus[2].uuid).NVLinks)
	for _, g := range topo.GPUs {
		require.Empty(t, g.NVMLError)
	}
}

func TestCollectNVLinkProbeNoError(t *testing.T) {
	gpus := eightGPUs()[:2]
	// An RTX 3090 without a bridge: links 0-3 FEATURE_DISABLED, link 4 ERROR_INVALID_ARGUMENT.
	rtx3090 := func(link int) (nvml.EnableState, nvml.Return, string, nvml.Return) {
		require.LessOrEqual(t, link, 4, "no call above link 4")
		if link < 4 {
			return nvml.FEATURE_DISABLED, nvml.SUCCESS, "", nvml.SUCCESS
		}
		return nvml.FEATURE_DISABLED, nvml.ERROR_INVALID_ARGUMENT, "", nvml.SUCCESS
	}
	gpus[0].nvlink = rtx3090
	// gpus[1] has no NVLink: ERROR_NOT_SUPPORTED for link 0.
	f := &fakeNode{t: t, gpus: gpus}
	topo := collectFake(f, inventoryOf(gpus))

	require.Zero(t, topo.Links[0].NVLinks)
	for _, g := range topo.GPUs {
		require.Empty(t, g.NVMLError)
	}
	require.Equal(t, []int{0, 1, 2, 3, 4}, f.nvlinkCalls[0])
	require.Equal(t, []int{0}, f.nvlinkCalls[1])
}

func TestNVMLReturnFormatting(t *testing.T) {
	cases := map[nvml.Return]string{
		nvml.SUCCESS:                 "SUCCESS (0)",
		nvml.ERROR_NOT_SUPPORTED:     "ERROR_NOT_SUPPORTED (3)",
		nvml.ERROR_LIBRARY_NOT_FOUND: "ERROR_LIBRARY_NOT_FOUND (12)",
		nvml.ERROR_GPU_IS_LOST:       "ERROR_GPU_IS_LOST (15)",
		nvml.ERROR_NOT_READY:         "ERROR_NOT_READY (27)",
		nvml.ERROR_GPU_NOT_FOUND:     "ERROR_GPU_NOT_FOUND (28)",
		nvml.ERROR_INVALID_STATE:     "ERROR_INVALID_STATE (29)",
		nvml.ERROR_UNKNOWN:           "ERROR_UNKNOWN (999)",
		30:                           "UNKNOWN_RETURN (30)",
		-1:                           "UNKNOWN_RETURN (-1)",
	}
	for ret, want := range cases {
		require.Equal(t, want, nvmlReturnString(ret))
	}
	// Every Return constant of go-nvml (const.go): SUCCESS, 1-29 and ERROR_UNKNOWN.
	require.Len(t, nvmlReturnNames, 31)
	for code := nvml.Return(0); code <= 29; code++ {
		require.NotEqual(t, "UNKNOWN_RETURN", nvmlReturnName(code), "code %d", code)
	}
}

func TestCollectErrorsIgnoreNVMLProse(t *testing.T) {
	// go-nvml's own strings differ from the names: its built-in table has no entry for 27-29,
	// and a loaded library returns NVML's prose. Collection must never use them.
	require.NotEqual(t, "ERROR_NOT_READY", nvml.ERROR_NOT_READY.Error())

	gpus := eightGPUs()[:2]
	gpus[0].rets = map[string]nvml.Return{
		"GetPciInfo":           nvml.ERROR_NOT_READY,
		"GetCurrPcieLinkWidth": nvml.ERROR_INVALID_STATE,
		"GetMaxPcieLinkWidth":  nvml.Return(1000),
	}
	f := &fakeNode{t: t, gpus: gpus, initRet: nvml.SUCCESS}
	topo := collectFake(f, inventoryOf(gpus))
	require.Equal(t,
		"GetPciInfo: ERROR_NOT_READY (27); GetCurrPcieLinkWidth: ERROR_INVALID_STATE (29); "+
			"GetMaxPcieLinkWidth: UNKNOWN_RETURN (1000)",
		topo.GPUs[0].NVMLError)

	f = &fakeNode{t: t, gpus: gpus, initRet: nvml.ERROR_GPU_NOT_FOUND}
	topo = collectFake(f, inventoryOf(gpus))
	require.Equal(t, "NVML init: ERROR_GPU_NOT_FOUND (28)", topo.UnknownReason)
}

func TestCollectGPUsInitsOnce(t *testing.T) {
	gpus := eightGPUs()
	devices, excluded := slotsAndExcluded(gpus, 7)
	for _, probe := range []bool{false, true} {
		f := &fakeNode{t: t, gpus: gpus, driver: "610.57.04"}
		c := collectGPUs(devices, excluded, probe, f.session(), time.Minute)

		require.Equal(t, "SUCCESS", c.NVMLInit)
		require.Zero(t, c.NVMLInitCode)
		require.Equal(t, "610.57.04", c.DriverVersion)
		require.Empty(t, c.Topology.UnknownReason)
		require.Equal(t, "610.57.04", c.Topology.DriverVersion)
		require.Len(t, c.Topology.GPUs, 8)
		require.Len(t, c.Topology.Links, 28)
		inits, shutdowns := f.counts()
		require.Equal(t, 1, inits, "one Init for the driver version and the measurement")
		require.Equal(t, 1, shutdowns)
	}
}

func TestCollectGPUsWithoutGPUsReportsLibrary(t *testing.T) {
	// The subcommand in a GPU-less image: NVML is loaded with no GPU to measure.
	f := &fakeNode{t: t, initRet: nvml.ERROR_LIBRARY_NOT_FOUND}
	require.Equal(t, GPUCollection{NVMLInit: "ERROR_LIBRARY_NOT_FOUND", NVMLInitCode: 12},
		collectGPUs(nil, nil, true, f.session(), time.Minute))
	inits, shutdowns := f.counts()
	require.Equal(t, 1, inits)
	require.Zero(t, shutdowns)

	f = &fakeNode{t: t, driver: "610.57.04"}
	require.Equal(t, GPUCollection{NVMLInit: "SUCCESS", DriverVersion: "610.57.04"},
		collectGPUs(nil, nil, true, f.session(), time.Minute))
	inits, shutdowns = f.counts()
	require.Equal(t, 1, inits)
	require.Equal(t, 1, shutdowns)
	require.Empty(t, f.handleRequests)
}

func TestCollectGPUsBlockingInit(t *testing.T) {
	gpus := eightGPUs()
	devices, excluded := slotsAndExcluded(gpus, 7)
	f := &fakeNode{t: t, gpus: gpus, initBlock: make(chan struct{})}
	t.Cleanup(func() {
		close(f.initBlock)
		waitSessionDone()
		inits, _ := f.counts()
		require.Equal(t, 1, inits, "Init ran once, also after it returned")
	})

	start := time.Now()
	c := collectGPUs(devices, excluded, true, f.session(), 50*time.Millisecond)
	require.Less(t, time.Since(start), 10*time.Second)
	require.Equal(t, "TIMEOUT", c.NVMLInit)
	require.Equal(t, -2, c.NVMLInitCode)
	require.Empty(t, c.DriverVersion)
	require.Equal(t, "NVML did not finish within 0.05s", c.Topology.UnknownReason)
	require.Equal(t, inventoryOf(gpus, 7), c.Topology.GPUs, "the slots and the excluded GPU, unmeasured")

	// The first Init is still blocked: a second collection must not start another one.
	c = collectGPUs(devices, excluded, true, f.session(), time.Minute)
	require.Equal(t, "TIMEOUT", c.NVMLInit)
	require.Equal(t, "an earlier NVML session has not finished", c.Topology.UnknownReason)
	require.Equal(t, inventoryOf(gpus, 7), c.Topology.GPUs)
	require.Eventually(t, func() bool {
		inits, _ := f.counts()
		return inits == 1
	}, 10*time.Second, time.Millisecond)
}

func TestCollectPanicKeepsInventory(t *testing.T) {
	gpus := eightGPUs()[:3]
	gpus[1].panics = true
	devices, excluded := slotsAndExcluded(gpus, 2)
	f := &fakeNode{t: t, gpus: gpus, driver: "610.57.04"}
	c := collectGPUs(devices, excluded, false, f.session(), time.Minute)
	require.Equal(t, "SUCCESS", c.NVMLInit)
	require.Equal(t, &aproto.GPUTopology{
		UnknownReason: "NVML collection failed",
		GPUs:          inventoryOf(gpus, 2),
	}, c.Topology, "the inventory without the telemetry measured before the panic")
	_, shutdowns := f.counts()
	require.Equal(t, 1, shutdowns)
}

func TestNormalizeBusID(t *testing.T) {
	require.Equal(t, "0000:a1:00.0", normalizeBusID("00000000:A1:00.0"))
	require.Equal(t, "0000:01:00.0", normalizeBusID("0000:01:00.0"))
	require.Equal(t, "0001:c1:00.0", normalizeBusID("00000001:C1:00.0"))
	require.Equal(t, "garbage", normalizeBusID("GARBAGE"))
	info := pciInfo("00000000:41:00.0")
	require.Equal(t, "00000000:41:00.0", busIDString(info.BusId[:]))
}

func TestNUMANodeReader(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, path), []byte(content), 0o600))
	}
	pci := filepath.Join(root, "bus/pci/devices")
	write("bus/pci/devices/0000:41:00.0/numa_node", "0\n")
	write("bus/pci/devices/0000:c1:00.0/numa_node", "1\n")
	write("bus/pci/devices/0000:01:00.0/numa_node", "-1\n")
	write("single/online", "0\n")
	write("dual/online", "0-1\n")
	node := func(numa func(string) *int, bdf string) any {
		if n := numa(bdf); n != nil {
			return *n
		}
		return nil
	}

	cases := []struct {
		online    string
		minusOne  any // what numa_node -1 becomes
		rationale string
	}{
		{"single/online", 0, "one NUMA node online: -1 is node 0"},
		{"dual/online", nil, "several NUMA nodes online: -1 stays unknown"},
		{"missing/online", nil, "the online nodes are unreadable: -1 stays unknown"},
	}
	for _, c := range cases {
		numa := numaNodeReader(pci, filepath.Join(root, c.online))
		require.Equal(t, c.minusOne, node(numa, "0000:01:00.0"), c.rationale)
		require.Equal(t, 0, node(numa, "0000:41:00.0"), "0 stays 0")
		require.Equal(t, 1, node(numa, "0000:c1:00.0"), "1 stays 1")
		require.Nil(t, node(numa, "0000:02:00.0"), "a read error is unknown")
		require.Nil(t, node(numa, "../etc"))
		require.Nil(t, node(numa, ""))
	}
}

func TestCollectGPUsInventoryWithoutTelemetry(t *testing.T) {
	// 8 GPUs detected, 1 excluded; NVML init fails or does not return.
	gpus := eightGPUs()
	devices, excluded := slotsAndExcluded(gpus, 7)
	want := inventoryOf(gpus, 7)

	f := &fakeNode{t: t, gpus: gpus, initRet: nvml.ERROR_DRIVER_NOT_LOADED}
	c := collectGPUs(devices, excluded, false, f.session(), time.Minute)
	require.Equal(t, "ERROR_DRIVER_NOT_LOADED", c.NVMLInit)
	require.Equal(t, 9, c.NVMLInitCode)
	require.Equal(t, "NVML init: ERROR_DRIVER_NOT_LOADED (9)", c.Topology.UnknownReason)
	require.Equal(t, want, c.Topology.GPUs)

	f = &fakeNode{t: t, gpus: gpus, initBlock: make(chan struct{})}
	t.Cleanup(func() {
		close(f.initBlock)
		waitSessionDone()
	})
	c = collectGPUs(devices, excluded, false, f.session(), 20*time.Millisecond)
	require.Equal(t, "NVML did not finish within 0.02s", c.Topology.UnknownReason)
	require.Equal(t, want, c.Topology.GPUs)
}
