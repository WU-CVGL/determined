package containers

import (
	"context"
	"syscall"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/agent/internal/container"
	"github.com/determined-ai/determined/agent/internal/options"
	"github.com/determined-ai/determined/agent/pkg/docker"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

func labelsWithUUIDs(devices, uuids string) types.Container {
	c := labelsWithoutUUIDs(devices)
	c.Labels[docker.ContainerDeviceUUIDsLabel] = uuids
	return c
}

func TestContainerDockerLabelsRoundTrip(t *testing.T) {
	m := &Manager{devices: sparseGPUs(0, 1, 2, 3, 5, 6, 7)}
	for _, devices := range [][]device.Device{sparseGPUs(7, 5), sparseGPUs(2), {}} {
		cont := cproto.Container{ID: "c1", Description: "test", Devices: devices}
		labels := makeContainerDockerLabels(cont)

		got, err := m.unmakeContainerDockerLabels(types.Container{State: "running", Labels: labels})
		require.NoError(t, err)
		require.Equal(t, cont.ID, got.ID)
		require.Equal(t, cont.Description, got.Description)
		require.Equal(t, devices, got.Devices)
	}
}

func TestMakeContainerDockerLabelsDevices(t *testing.T) {
	labels := makeContainerDockerLabels(cproto.Container{Devices: sparseGPUs(7, 5)})
	require.Equal(t, "7,5", labels[docker.ContainerDevicesLabel])
	require.Equal(t, "GPU-7,GPU-5", labels[docker.ContainerDeviceUUIDsLabel])
}

func TestUnmakeContainerDockerLabelsUUIDs(t *testing.T) {
	m := &Manager{devices: sparseGPUs(0, 1, 2, 3, 5, 6, 7)}

	c, err := m.unmakeContainerDockerLabels(labelsWithUUIDs("5,7", "GPU-5,GPU-7"))
	require.NoError(t, err)
	require.Equal(t, sparseGPUs(5, 7), c.Devices)

	for _, tc := range []struct {
		name, devices, uuids, err string
	}{
		// Index 5 is another physical GPU now, for example after a GPU was removed and the host
		// rebooted.
		{"mismatch", "5", "GPU-other", "has UUID GPU-other"},
		{"swapped", "5,7", "GPU-7,GPU-5", "has UUID GPU-7"},
		{"count", "5,7", "GPU-5", "2 device IDs and 1 device UUIDs"},
		{"unknown id", "4", "GPU-4", "is not a device of this agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.unmakeContainerDockerLabels(labelsWithUUIDs(tc.devices, tc.uuids))
			require.ErrorContains(t, err, tc.err)
		})
	}
}

func TestUnmakeContainerDockerLabelsZeroSlotWithUUIDs(t *testing.T) {
	m := &Manager{devices: sparseGPUs(0, 1)}
	c, err := m.unmakeContainerDockerLabels(labelsWithUUIDs("", ""))
	require.NoError(t, err)
	require.Empty(t, c.Devices)
}

// killRecorder is a container runtime that lists the given containers and records kills.
type killRecorder struct {
	container.ContainerRuntime
	running map[cproto.ID]types.Container
	killed  []string
}

func (k *killRecorder) ListRunningContainers(
	context.Context, filters.Args,
) (map[cproto.ID]types.Container, error) {
	return k.running, nil
}

func (k *killRecorder) SignalContainer(_ context.Context, id string, sig syscall.Signal) error {
	if sig == syscall.SIGKILL {
		k.killed = append(k.killed, id)
	}
	return nil
}

func TestReattachContainersKillsContainersWithUnknownDevices(t *testing.T) {
	unknownID := labelsWithUUIDs("4", "GPU-4")
	unknownID.ID = "docker-unknown-id"
	unknownID.Labels[docker.ContainerIDLabel] = "unknown-id"
	otherGPU := labelsWithUUIDs("5", "GPU-other")
	otherGPU.ID = "docker-other-gpu"
	otherGPU.Labels[docker.ContainerIDLabel] = "other-gpu"
	runtime := &killRecorder{running: map[cproto.ID]types.Container{
		"unknown-id": unknownID,
		"other-gpu":  otherGPU,
	}}
	m, err := New(options.Options{AgentID: "node01"}, aproto.MasterSetAgentOptions{},
		sparseGPUs(0, 1, 2, 3, 5, 6, 7), runtime, nil)
	require.NoError(t, err)

	acks, err := m.ReattachContainers(context.Background(), []aproto.ContainerReattach{
		{Container: cproto.Container{ID: "unknown-id", State: cproto.Running}},
		{Container: cproto.Container{ID: "other-gpu", State: cproto.Running}},
	})
	require.NoError(t, err)
	require.Len(t, acks, 2)
	for _, ack := range acks {
		require.NotNil(t, ack.Failure, ack.Container.ID)
		require.Equal(t, aproto.RestoreError, ack.Failure.FailureType)
	}
	require.ElementsMatch(t, []string{"docker-unknown-id", "docker-other-gpu"}, runtime.killed)
	require.Zero(t, m.NumContainers())
}
