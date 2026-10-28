package containers

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/agent/internal/options"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

func TestOverwriteSpecCUDAPreservesDriverCapabilities(t *testing.T) {
	for _, capabilities := range []string{"all", "compute,utility,graphics,display"} {
		t.Run(capabilities, func(t *testing.T) {
			spec := cproto.Spec{}
			spec.RunSpec.HostConfig.Runtime = "runc"
			requested := "NVIDIA_DRIVER_CAPABILITIES=" + capabilities
			spec.RunSpec.ContainerConfig.Env = []string{requested}
			cont := cproto.Container{Devices: []device.Device{
				{ID: 3, UUID: "GPU-allocated", Type: device.CUDA},
			}}

			got, err := overwriteSpec(spec, cont, options.Options{}, aproto.MasterSetAgentOptions{})
			require.NoError(t, err)
			require.Equal(t, "nvidia", got.RunSpec.HostConfig.Runtime)
			require.Contains(t, got.RunSpec.ContainerConfig.Env, requested)
			require.Len(t, got.RunSpec.HostConfig.DeviceRequests, 1)
			request := got.RunSpec.HostConfig.DeviceRequests[0]
			require.Equal(t, []string{"GPU-allocated"}, request.DeviceIDs)
			require.Zero(t, request.Count)
			// Driver capabilities here would override the task or image's environment.
			require.Equal(t, [][]string{{"gpu"}}, request.Capabilities)
			require.False(t, got.RunSpec.HostConfig.Privileged)
			require.Empty(t, got.RunSpec.HostConfig.CapAdd)
			require.Empty(t, got.RunSpec.HostConfig.Binds)
		})
	}
}

func TestOverwriteSpecCPUDoesNotSelectNvidiaRuntime(t *testing.T) {
	spec := cproto.Spec{}
	spec.RunSpec.HostConfig.Runtime = "custom-cpu-runtime"

	got, err := overwriteSpec(spec, cproto.Container{}, options.Options{}, aproto.MasterSetAgentOptions{})
	require.NoError(t, err)
	require.Equal(t, "custom-cpu-runtime", got.RunSpec.HostConfig.Runtime)
	require.Empty(t, got.RunSpec.HostConfig.DeviceRequests)
}
