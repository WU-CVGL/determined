package containers

import (
	"fmt"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/agent/pkg/docker"
	"github.com/determined-ai/determined/master/pkg/device"
)

// sparseGPUs returns CUDA devices with the given IDs, as an agent with visible_gpus or
// exclude_gpus reports them.
func sparseGPUs(ids ...int) []device.Device {
	var devices []device.Device
	for _, id := range ids {
		devices = append(devices, device.Device{
			ID: device.ID(id), Brand: "test", UUID: fmt.Sprintf("GPU-%d", id), Type: device.CUDA,
		})
	}
	return devices
}

// labelsWithoutUUIDs returns the labels of a container created with the given device IDs by an
// agent that wrote no UUIDs label.
func labelsWithoutUUIDs(devices string) types.Container {
	return types.Container{
		State: "running",
		Labels: map[string]string{
			docker.ContainerVersionLabel:     docker.ContainerVersionValue,
			docker.ContainerIDLabel:          "c1",
			docker.ContainerDescriptionLabel: "test",
			docker.ContainerDevicesLabel:     devices,
		},
	}
}

func TestUnmakeContainerDockerLabelsSparseIDs(t *testing.T) {
	// GPU 4 excluded: the agent's devices are 0-3 and 5-7.
	m := &Manager{devices: sparseGPUs(0, 1, 2, 3, 5, 6, 7)}

	for _, tc := range []struct {
		label string
		want  []device.Device
	}{
		{"5", sparseGPUs(5)},
		{"7", sparseGPUs(7)},
		{"5,7", sparseGPUs(5, 7)},
		{"0,3,6", sparseGPUs(0, 3, 6)},
	} {
		t.Run(tc.label, func(t *testing.T) {
			c, err := m.unmakeContainerDockerLabels(labelsWithoutUUIDs(tc.label))
			require.NoError(t, err)
			require.Equal(t, tc.want, c.Devices)
		})
	}
}

func TestUnmakeContainerDockerLabelsUnknownID(t *testing.T) {
	m := &Manager{devices: sparseGPUs(0, 1, 2, 3, 5, 6, 7)}
	for _, label := range []string{"4", "8", "5,4", "100"} {
		t.Run(label, func(t *testing.T) {
			_, err := m.unmakeContainerDockerLabels(labelsWithoutUUIDs(label))
			require.ErrorContains(t, err, "is not a device of this agent")
		})
	}
}

func TestUnmakeContainerDockerLabelsZeroSlot(t *testing.T) {
	m := &Manager{devices: sparseGPUs(0, 1)}
	c, err := m.unmakeContainerDockerLabels(labelsWithoutUUIDs(""))
	require.NoError(t, err)
	require.Empty(t, c.Devices)
}
