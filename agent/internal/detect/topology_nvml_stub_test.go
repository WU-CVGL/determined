//go:build !linux || !cgo

package detect

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// The stub build (CGO_ENABLED=0, or not Linux) reports the inventory as unknown (N6, D21).
func TestDetectGPUTopologyStub(t *testing.T) {
	devices := cudaDevices("GPU-0", "GPU-1", "GPU-2", "GPU-3", "GPU-5", "GPU-6", "GPU-7")
	excluded := []device.Device{{ID: 4, UUID: "GPU-4", Type: device.CUDA}}

	topo := DetectGPUTopology(devices, excluded)
	require.Equal(t, "agent built without NVML support (needs linux and cgo)", topo.UnknownReason)
	require.Equal(t, []aproto.GPUInfo{
		{UUID: "GPU-0"},
		{UUID: "GPU-1"},
		{UUID: "GPU-2"},
		{UUID: "GPU-3"},
		{UUID: "GPU-5"},
		{UUID: "GPU-6"},
		{UUID: "GPU-7"},
		{UUID: "GPU-4", Excluded: true},
	}, topo.GPUs)
	require.Empty(t, topo.Links)
	require.Nil(t, topo.CollectedAt)

	require.Nil(t, DetectGPUTopology(nil, nil))

	c := CollectGPUs(devices, excluded)
	require.Equal(t, "NOT_BUILT", c.NVMLInit)
	require.Equal(t, -1, c.NVMLInitCode)
	require.Equal(t, topo, c.Topology)
	require.Equal(t, GPUCollection{NVMLInit: "NOT_BUILT", NVMLInitCode: -1}, CollectGPUs(nil, nil))
}
