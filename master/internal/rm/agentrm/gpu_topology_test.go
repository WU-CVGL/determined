package agentrm

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

var testGPULog = logrus.WithField("component", "gpu-topology-test")

func cudaSlots(uuids ...string) []device.Device {
	var devices []device.Device
	for i, u := range uuids {
		devices = append(devices, device.Device{ID: device.ID(i), Brand: "test", UUID: u, Type: device.CUDA})
	}
	return devices
}

var (
	p2pOK  = aproto.GPUP2PCaps{Read: aproto.GPUP2PStatusOK, Write: aproto.GPUP2PStatusOK}
	p2pNSW = aproto.GPUP2PCaps{Read: aproto.GPUP2PStatusOK, Write: aproto.GPUP2PStatusNotSupported}
)

func TestNewGPUTopologyMapsUUIDsToDeviceIDs(t *testing.T) {
	// Device IDs do not follow UUID order: slot 0 is GPU-z, slot 1 is GPU-a.
	devices := cudaSlots("GPU-z", "GPU-a", "GPU-m")
	devices = append(devices, device.Device{ID: 9, UUID: "cpu-0", Type: device.CPU})
	zero := 0
	collected := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	wire := &aproto.GPUTopology{
		CollectedAt:   &collected,
		DriverVersion: "610.57.04",
		GPUs: []aproto.GPUInfo{
			{UUID: "GPU-z", PCIBusID: "0000:41:00.0", NUMANode: &zero, PCIeLinkWidth: 8, PCIeLinkWidthMax: 16},
			{UUID: "GPU-a", PCIBusID: "0000:01:00.0"},
			{UUID: "GPU-m", Excluded: true}, // marked excluded but a slot: treated as a slot
			{UUID: "GPU-unknown"},           // neither a slot nor excluded: dropped
			{UUID: "cpu-0"},                 // not a CUDA device: dropped
		},
		Links: []aproto.GPULink{
			// Wire order (UUIDA < UUIDB) is GPU-a, GPU-z: slot 1 then slot 0. The master keys it as
			// (0, 1) and swaps the directions with it.
			{UUIDA: "GPU-a", UUIDB: "GPU-z", Level: aproto.GPULinkLevelNode, P2PAToB: p2pOK, P2PBToA: p2pNSW},
			// A reversed pair on the wire is normalized the same way.
			{UUIDA: "GPU-z", UUIDB: "GPU-m", Level: aproto.GPULinkLevelSys, NVLinks: 2, P2PAToB: p2pNSW, P2PBToA: p2pOK},
			{UUIDA: "GPU-a", UUIDB: "GPU-a"},                                // self: dropped
			{UUIDA: "GPU-a", UUIDB: "GPU-unknown", Level: "PIX"},            // not slots: dropped
			{UUIDA: "GPU-a", UUIDB: "GPU-z", Level: aproto.GPULinkLevelSys}, // duplicate: first wins
		},
	}
	g := newGPUTopology(wire, devices, "0.42.0", testGPULog)

	require.Empty(t, g.unknownReason)
	require.Equal(t, collected, g.collectedAt)
	require.Equal(t, "610.57.04", g.driverVersion)
	require.Len(t, g.gpus, 3)
	require.Equal(t, "GPU-z", g.gpus[0].UUID)
	require.Equal(t, 8, g.gpus[0].PCIeLinkWidth)
	require.Equal(t, "GPU-a", g.gpus[1].UUID)
	require.False(t, g.gpus[2].Excluded, "a slot is never excluded")
	require.Empty(t, g.excluded)
	require.Empty(t, g.excludedLinks)

	require.Equal(t, map[gpuPairKey]gpuPair{
		{a: 0, b: 1}: {level: aproto.GPULinkLevelNode, p2pAToB: p2pNSW, p2pBToA: p2pOK},
		{a: 0, b: 2}: {level: aproto.GPULinkLevelSys, nvlinks: 2, p2pAToB: p2pNSW, p2pBToA: p2pOK},
	}, g.pairs)
}

func TestNewGPUTopologyUnknown(t *testing.T) {
	devices := cudaSlots("GPU-a", "GPU-b")

	g := newGPUTopology(nil, devices, "0.41.0", testGPULog)
	require.Equal(t, "agent 0.41.0 does not report GPU topology", g.unknownReason)
	require.Empty(t, g.gpus)
	require.Empty(t, g.pairs)

	cpu := []device.Device{{ID: 0, UUID: "cpu", Type: device.CPU}}
	require.Empty(t, newGPUTopology(nil, cpu, "0.41.0", testGPULog).unknownReason)

	g = newGPUTopology(&aproto.GPUTopology{
		UnknownReason: "NVML init: ERROR_LIBRARY_NOT_FOUND (12)",
		GPUs:          []aproto.GPUInfo{{UUID: "GPU-a"}, {UUID: "GPU-b"}},
	}, devices, "0.42.0", testGPULog)
	require.Equal(t, "NVML init: ERROR_LIBRARY_NOT_FOUND (12)", g.unknownReason)
	require.Len(t, g.gpus, 2)
}

func TestGPUTopologyKeepsExcludedForDisplay(t *testing.T) {
	// node01 with the exclude list: slots are host GPUs 0-3 and 5-7; 81:00.0 (GPU-4) is excluded.
	devices := []device.Device{}
	for _, i := range []int{0, 1, 2, 3, 5, 6, 7} {
		devices = append(devices, device.Device{
			ID: device.ID(i), UUID: "GPU-" + string(rune('0'+i)), Type: device.CUDA,
		})
	}

	// Also when the report is unknown (N6).
	unknown := &aproto.GPUTopology{UnknownReason: "NVML did not finish within 60s"}
	for _, d := range devices {
		unknown.GPUs = append(unknown.GPUs, aproto.GPUInfo{UUID: d.UUID})
	}
	unknown.GPUs = append(unknown.GPUs, aproto.GPUInfo{UUID: "GPU-4", Excluded: true})
	g := newGPUTopology(unknown, devices, "0.42.0", testGPULog)
	require.Len(t, g.gpus, 7)
	require.Equal(t, []aproto.GPUInfo{{UUID: "GPU-4", Excluded: true}}, g.excluded)
	_, isSlot := g.gpus[4]
	require.False(t, isSlot, "an excluded GPU is never a slot")

	known := &aproto.GPUTopology{GPUs: append([]aproto.GPUInfo(nil), unknown.GPUs...)}
	known.GPUs[7] = aproto.GPUInfo{
		UUID: "GPU-4", PCIBusID: "0000:81:00.0", Excluded: true, PCIeLinkWidth: 16, PCIeLinkWidthMax: 16,
	}
	known.Links = []aproto.GPULink{
		{UUIDA: "GPU-3", UUIDB: "GPU-4", Level: aproto.GPULinkLevelNode, P2PAToB: p2pOK, P2PBToA: p2pNSW},
		// Reversed on the wire: normalized to UUIDA < UUIDB with the directions swapped.
		{UUIDA: "GPU-5", UUIDB: "GPU-4", Level: aproto.GPULinkLevelSys, P2PAToB: p2pNSW, P2PBToA: p2pOK},
		{UUIDA: "GPU-0", UUIDB: "GPU-1", Level: aproto.GPULinkLevelNode, P2PAToB: p2pOK, P2PBToA: p2pOK},
	}
	g = newGPUTopology(known, devices, "0.42.0", testGPULog)
	require.Equal(t, []aproto.GPUInfo{known.GPUs[7]}, g.excluded)
	require.Equal(t, []aproto.GPULink{
		{UUIDA: "GPU-3", UUIDB: "GPU-4", Level: aproto.GPULinkLevelNode, P2PAToB: p2pOK, P2PBToA: p2pNSW},
		{UUIDA: "GPU-4", UUIDB: "GPU-5", Level: aproto.GPULinkLevelSys, P2PAToB: p2pOK, P2PBToA: p2pNSW},
	}, g.excludedLinks)
	require.Equal(t, map[gpuPairKey]gpuPair{
		{a: 0, b: 1}: {level: aproto.GPULinkLevelNode, p2pAToB: p2pOK, p2pBToA: p2pOK},
	}, g.pairs, "links with an excluded end never reach the slot pairs")
}

func TestAgentStateGPUTopologyStaysOutOfCopies(t *testing.T) {
	state := newAgentState("agent", 0)
	g := newGPUTopology(&aproto.GPUTopology{}, nil, "0.42.0", testGPULog)
	state.setGPUTopology(g)
	require.Same(t, g, state.gpuTopology)
	require.Nil(t, state.deepCopy().gpuTopology)
}

// The API reads the topology through summarize (section 5.2).
func TestSummarizeReportsGPUTopology(t *testing.T) {
	devices := cudaSlots("GPU-a", "GPU-b")
	a := &agent{id: "agent", agentState: stateWithSlots(devices)}
	a.agentState.setGPUTopology(newGPUTopology(&aproto.GPUTopology{
		GPUs: []aproto.GPUInfo{
			{UUID: "GPU-a", PCIeLinkWidth: 8, PCIeLinkWidthMax: 16},
			{UUID: "GPU-b"},
			{UUID: "GPU-x", Excluded: true},
		},
	}, devices, "0.42.0", testGPULog))

	topo := a.summarize().GPUTopology
	require.NotNil(t, topo)
	require.Len(t, topo.Gpus, 3)
	require.Equal(t, int32(8), topo.Gpus[0].PcieLinkWidth)
	require.Equal(t, int32(-1), topo.Gpus[2].DeviceId)
	require.True(t, topo.Gpus[2].Excluded)

	// Before the AgentStarted, no agent state and so no topology.
	require.Nil(t, (&agent{id: "agent"}).summarize().GPUTopology)
}

// stateWithSlots returns an agent state with the given devices as slots, as agentStarted builds it.
func stateWithSlots(devices []device.Device) *agentState {
	state := newAgentState("agent", 0)
	for _, d := range devices {
		state.slotStates[d.ID] = &slot{device: d, enabled: slotEnabled{agentEnabled: true, userEnabled: true}}
	}
	return state
}

func TestGPUTopologyProtoShapeWhenUnknown(t *testing.T) {
	devices := cudaSlots("GPU-a", "GPU-b", "GPU-c")

	// Restored from the snapshot, before the agent's AgentStarted.
	state := stateWithSlots(devices)
	topo := state.gpuTopologyProto()
	require.Equal(t, "not reported since the master started", topo.UnknownReason)
	require.Nil(t, topo.CollectedAt)
	require.Len(t, topo.Gpus, 3)
	for i, g := range topo.Gpus {
		require.Equal(t, int32(i), g.DeviceId)
		require.Equal(t, devices[i].UUID, g.Uuid)
		require.Equal(t, int32(-1), g.NumaNode)
		require.False(t, g.Excluded)
	}
	require.Empty(t, topo.Links)

	// An older agent.
	state.setGPUTopology(newGPUTopology(nil, devices, "0.41.0", testGPULog))
	require.Equal(t, "agent 0.41.0 does not report GPU topology", state.gpuTopologyProto().UnknownReason)

	// A CPU agent has none.
	cpu := stateWithSlots([]device.Device{{ID: 0, UUID: "cpu", Type: device.CPU}})
	require.Nil(t, cpu.gpuTopologyProto())
	cpu.setGPUTopology(newGPUTopology(nil, nil, "0.42.0", testGPULog))
	require.Nil(t, cpu.gpuTopologyProto())
}

// N6 at the master: 8 GPUs detected, 1 excluded, NVML unknown; and all 8 excluded.
func TestGPUTopologyProtoInventoryWithoutTelemetry(t *testing.T) {
	var devices []device.Device
	wire := &aproto.GPUTopology{UnknownReason: "NVML init: ERROR_LIBRARY_NOT_FOUND (12)"}
	for _, i := range []int{0, 1, 2, 3, 5, 6, 7} {
		u := "GPU-" + string(rune('0'+i))
		devices = append(devices, device.Device{ID: device.ID(i), UUID: u, Type: device.CUDA})
		wire.GPUs = append(wire.GPUs, aproto.GPUInfo{UUID: u})
	}
	wire.GPUs = append(wire.GPUs, aproto.GPUInfo{UUID: "GPU-4", Excluded: true})
	state := stateWithSlots(devices)
	state.setGPUTopology(newGPUTopology(wire, devices, "0.42.0", testGPULog))

	topo := state.gpuTopologyProto()
	require.Equal(t, "NVML init: ERROR_LIBRARY_NOT_FOUND (12)", topo.UnknownReason)
	require.Len(t, topo.Gpus, 8)
	var ids []int32
	for _, g := range topo.Gpus[:7] {
		ids = append(ids, g.DeviceId)
		require.False(t, g.Excluded)
	}
	require.Equal(t, []int32{0, 1, 2, 3, 5, 6, 7}, ids)
	require.Equal(t, int32(-1), topo.Gpus[7].DeviceId)
	require.Equal(t, "GPU-4", topo.Gpus[7].Uuid)
	require.True(t, topo.Gpus[7].Excluded)

	// All excluded, with telemetry: the agent has no slots and still reports them.
	all := &aproto.GPUTopology{DriverVersion: "610.57.04"}
	for i := 0; i < 8; i++ {
		all.GPUs = append(all.GPUs, aproto.GPUInfo{
			UUID: "GPU-" + string(rune('0'+i)), PCIBusID: "0000:" + string(rune('a'+i)) + "1:00.0",
			PCIeLinkWidth: 16, PCIeLinkWidthMax: 16, Excluded: true,
		})
	}
	none := stateWithSlots(nil)
	none.setGPUTopology(newGPUTopology(all, []device.Device{}, "0.42.0", testGPULog))
	topo = none.gpuTopologyProto()
	require.NotNil(t, topo)
	require.Empty(t, topo.UnknownReason)
	require.Len(t, topo.Gpus, 8)
	for _, g := range topo.Gpus {
		require.True(t, g.Excluded)
		require.Equal(t, int32(-1), g.DeviceId)
		require.Equal(t, int32(16), g.PcieLinkWidth)
	}
}

func TestGPUTopologyProtoOrderAndLinks(t *testing.T) {
	// Slot 0 is GPU-z and slot 1 is GPU-a; two excluded GPUs, one without a bus id.
	devices := cudaSlots("GPU-z", "GPU-a")
	one := 1
	collected := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	wire := &aproto.GPUTopology{
		CollectedAt:   &collected,
		DriverVersion: "610.57.04",
		GPUs: []aproto.GPUInfo{
			{UUID: "GPU-z", PCIBusID: "0000:41:00.0", NUMANode: &one, PCIeLinkWidth: 8, PCIeLinkWidthMax: 16},
			{UUID: "GPU-a", PCIBusID: "0000:01:00.0"},
			{UUID: "GPU-y", PCIBusID: "0000:81:00.0", Excluded: true},
			{UUID: "GPU-x", Excluded: true},
		},
		Links: []aproto.GPULink{
			{UUIDA: "GPU-a", UUIDB: "GPU-z", Level: aproto.GPULinkLevelNode, NVLinks: 2, P2PAToB: p2pOK, P2PBToA: p2pNSW},
			{UUIDA: "GPU-a", UUIDB: "GPU-y", Level: "FUTURE_LEVEL", P2PAToB: p2pOK, P2PBToA: aproto.GPUP2PCaps{
				Read: aproto.GPUP2PStatusOK, Write: "FUTURE_STATUS",
			}},
			{UUIDA: "GPU-x", UUIDB: "GPU-y", Level: aproto.GPULinkLevelSys, P2PAToB: p2pOK, P2PBToA: p2pOK},
		},
	}
	state := stateWithSlots(devices)
	state.setGPUTopology(newGPUTopology(wire, devices, "0.42.0", testGPULog))
	topo := state.gpuTopologyProto()

	require.Empty(t, topo.UnknownReason)
	require.Equal(t, "610.57.04", topo.DriverVersion)
	require.Equal(t, collected.Unix(), topo.CollectedAt.AsTime().Unix())

	// Slots by device_id, then excluded GPUs by bus id ("" first).
	var order []string
	for _, g := range topo.Gpus {
		order = append(order, g.Uuid)
	}
	require.Equal(t, []string{"GPU-z", "GPU-a", "GPU-x", "GPU-y"}, order)
	require.True(t, proto.Equal(&agentv1.GpuInfo{
		DeviceId: 0, Uuid: "GPU-z", PciBusId: "0000:41:00.0", NumaNode: 1,
		PcieLinkWidth: 8, PcieLinkWidthMax: 16,
	}, topo.Gpus[0]), topo.Gpus[0].String())

	require.Len(t, topo.Links, 3)
	// Between slots: device_a < device_b, with the directions swapped to match.
	slotLink := topo.Links[0]
	require.Equal(t, int32(0), slotLink.DeviceA)
	require.Equal(t, int32(1), slotLink.DeviceB)
	require.Equal(t, "GPU-z", slotLink.UuidA)
	require.Equal(t, "GPU-a", slotLink.UuidB)
	require.Equal(t, agentv1.GpuLinkLevel_GPU_LINK_LEVEL_NODE, slotLink.Level)
	require.Equal(t, int32(2), slotLink.Nvlinks)
	require.Equal(t, agentv1.GpuP2PStatus_GPU_P2P_STATUS_NOT_SUPPORTED, slotLink.P2PAToB.Write)
	require.Equal(t, agentv1.GpuP2PStatus_GPU_P2P_STATUS_OK, slotLink.P2PBToA.Write)
	require.Equal(t, agentv1.GpuP2P_GPU_P2P_NOT_USABLE, slotLink.P2P)

	// With an excluded end: -1 for that end, uuid_a < uuid_b; unknown strings degrade to unknown.
	require.Equal(t, int32(1), topo.Links[1].DeviceA)
	require.Equal(t, int32(-1), topo.Links[1].DeviceB)
	require.Equal(t, "GPU-a", topo.Links[1].UuidA)
	require.Equal(t, agentv1.GpuLinkLevel_GPU_LINK_LEVEL_UNSPECIFIED, topo.Links[1].Level)
	require.Equal(t, agentv1.GpuP2PStatus_GPU_P2P_STATUS_UNSPECIFIED, topo.Links[1].P2PBToA.Write)
	require.Equal(t, agentv1.GpuP2P_GPU_P2P_UNSPECIFIED, topo.Links[1].P2P)
	require.Equal(t, int32(-1), topo.Links[2].DeviceA)
	require.Equal(t, int32(-1), topo.Links[2].DeviceB)
	require.Equal(t, agentv1.GpuP2P_GPU_P2P_USABLE, topo.Links[2].P2P)
}
