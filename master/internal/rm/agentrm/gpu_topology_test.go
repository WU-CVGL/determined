package agentrm

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
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
		CollectedAt:   collected,
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
	unknown := &aproto.GPUTopology{UnknownReason: "NVML collection did not finish within 60s"}
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
