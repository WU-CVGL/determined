package detect

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

func cudaDevices(uuids ...string) []device.Device {
	var devices []device.Device
	for i, u := range uuids {
		devices = append(devices, device.Device{ID: device.ID(i), Brand: "test", UUID: u, Type: device.CUDA})
	}
	return devices
}

func mustNotCollect(t *testing.T) topologyCollector {
	return func([]aproto.GPUInfo) *aproto.GPUTopology {
		require.Fail(t, "the collector must not run")
		return nil
	}
}

func TestDetectGPUTopologyNoCUDA(t *testing.T) {
	require.Nil(t, detectGPUTopology(nil, nil, mustNotCollect(t), time.Minute))
	cpu := []device.Device{{ID: 0, Brand: "cpu", UUID: "cpu", Type: device.CPU}}
	require.Nil(t, detectGPUTopology(cpu, nil, mustNotCollect(t), time.Minute))
	rocm := []device.Device{{ID: 0, Brand: "amd", UUID: "0x7e", Type: device.ROCM}}
	require.Nil(t, detectGPUTopology(rocm, nil, mustNotCollect(t), time.Minute))

	// An agent whose GPUs are all excluded still reports them.
	collected := false
	topo := detectGPUTopology(nil, cudaDevices("GPU-a"),
		func(inv []aproto.GPUInfo) *aproto.GPUTopology {
			collected = true
			return &aproto.GPUTopology{GPUs: inv}
		}, time.Minute)
	require.True(t, collected)
	require.Equal(t, []aproto.GPUInfo{{UUID: "GPU-a", Excluded: true}}, topo.GPUs)
}

func TestDetectGPUTopologyMIG(t *testing.T) {
	devices := cudaDevices("MIG-1111", "MIG-2222")
	topo := detectGPUTopology(devices, nil, mustNotCollect(t), time.Minute)
	require.Equal(t, &aproto.GPUTopology{
		UnknownReason: "MIG instances: GPU topology not collected",
		GPUs:          []aproto.GPUInfo{{UUID: "MIG-1111"}, {UUID: "MIG-2222"}},
	}, topo)
}

func TestCollectTimeout(t *testing.T) {
	devices := cudaDevices("GPU-a", "GPU-b")
	excluded := []device.Device{{ID: 2, UUID: "GPU-c", Type: device.CUDA}}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var given []aproto.GPUInfo
	started := make(chan struct{})
	topo := detectGPUTopology(devices, excluded, func(inv []aproto.GPUInfo) *aproto.GPUTopology {
		given = inv
		close(started)
		<-release // a cgo call that never returns
		inv[0].PCIBusID = "written after the timeout"
		return &aproto.GPUTopology{GPUs: inv}
	}, 50*time.Millisecond)
	<-started

	require.Equal(t, "NVML collection did not finish within 0.05s", topo.UnknownReason)
	require.Equal(t, []aproto.GPUInfo{
		{UUID: "GPU-a"}, {UUID: "GPU-b"}, {UUID: "GPU-c", Excluded: true},
	}, topo.GPUs)
	require.NotSame(t, &given[0], &topo.GPUs[0], "the collector works on its own copy")

	require.Equal(t, "NVML collection did not finish within 60s", timeoutReason(gpuTopologyTimeout))
}

func TestCollectPanicKeepsInventory(t *testing.T) {
	topo := detectGPUTopology(cudaDevices("GPU-a"), nil, func([]aproto.GPUInfo) *aproto.GPUTopology {
		panic("index out of range")
	}, time.Minute)
	require.Equal(t, &aproto.GPUTopology{
		UnknownReason: "NVML collection failed", GPUs: []aproto.GPUInfo{{UUID: "GPU-a"}},
	}, topo)
}

func TestGPUTopologySummary(t *testing.T) {
	zero, one := 0, 1
	topo := &aproto.GPUTopology{
		DriverVersion: "610.57.04",
		GPUs: []aproto.GPUInfo{
			{UUID: "GPU-a", NUMANode: &zero, PCIeLinkWidth: 16, PCIeLinkWidthMax: 16},
			{UUID: "GPU-b", NUMANode: &zero, PCIeLinkWidth: 8, PCIeLinkWidthMax: 16},
			{UUID: "GPU-c", NUMANode: &one, PCIeLinkWidth: 16, PCIeLinkWidthMax: 16},
			{
				UUID: "GPU-d", PCIBusID: "0000:81:00.0", Excluded: true,
				NVMLError: "GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)",
			},
		},
	}
	ok := aproto.GPUP2PCaps{Read: aproto.GPUP2PStatusOK, Write: aproto.GPUP2PStatusOK}
	for _, p := range [][2]string{{"GPU-a", "GPU-b"}, {"GPU-a", "GPU-c"}, {"GPU-b", "GPU-c"}} {
		level := aproto.GPULinkLevelSys
		if p == [2]string{"GPU-a", "GPU-b"} {
			level = aproto.GPULinkLevelNode
		}
		topo.Links = append(topo.Links, aproto.GPULink{
			UUIDA: p[0], UUIDB: p[1], Level: level, P2PAToB: ok, P2PBToA: ok,
		})
	}
	line, hasErrors := gpuTopologySummary(topo, cudaDevices("GPU-a", "GPU-b", "GPU-c"))
	require.True(t, hasErrors)
	require.Equal(t, "GPU topology: 4 GPUs (1 excluded), NUMA 2+1, levels NODE/SYS, P2P usable, "+
		"width below max at start: slot 1 (x8 of x16), "+
		"NVML errors: excluded 0000:81:00.0 (GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)), "+
		"driver 610.57.04", line)
}
