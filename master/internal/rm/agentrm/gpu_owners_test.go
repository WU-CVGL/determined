package agentrm

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

func gpu(id int, uuid string) device.Device {
	return device.Device{ID: device.ID(id), Brand: "test", UUID: uuid, Type: device.CUDA}
}

func TestGPUOwnersClaim(t *testing.T) {
	g := newGPUOwners()
	node01 := &agent{id: "node01"}
	node09 := &agent{id: "node09"}

	require.NoError(t, g.claim(node01, []device.Device{gpu(0, "GPU-a"), gpu(1, "GPU-b")}))

	// Another agent ID reporting a GPU of node01 is refused, and claims none of its GPUs.
	err := g.claim(node09, []device.Device{gpu(0, "GPU-c"), gpu(1, "GPU-b"), gpu(2, "GPU-a")})
	require.EqualError(t, err,
		"agent node09 refused: GPU GPU-a,GPU-b is already reported by agent node01")
	require.Equal(t, map[string]*agent{"GPU-a": node01, "GPU-b": node01}, g.owners)

	// The same agent again, as on a reconnect, is accepted.
	require.NoError(t, g.claim(node01, []device.Device{gpu(0, "GPU-a"), gpu(1, "GPU-b")}))

	// So is a new agent with the same ID: it takes the GPUs over, and the old agent's release
	// leaves them with it.
	node01b := &agent{id: "node01"}
	require.NoError(t, g.claim(node01b, []device.Device{gpu(0, "GPU-a")}))
	g.release(node01)
	require.Equal(t, map[string]*agent{"GPU-a": node01b}, g.owners)

	// Once released, the GPU can go to another agent.
	g.release(node01b)
	require.NoError(t, g.claim(node09, []device.Device{gpu(0, "GPU-a")}))
}

func TestGPUOwnersConflictsWithSeveralAgents(t *testing.T) {
	g := newGPUOwners()
	require.NoError(t, g.claim(&agent{id: "b"}, []device.Device{gpu(0, "GPU-2")}))
	require.NoError(t, g.claim(&agent{id: "a"}, []device.Device{gpu(0, "GPU-1")}))
	err := g.claim(&agent{id: "c"}, []device.Device{gpu(0, "GPU-2"), gpu(1, "GPU-1")})
	require.EqualError(t, err, "agent c refused: GPU GPU-1 is already reported by agent a; "+
		"GPU GPU-2 is already reported by agent b")
}

func TestGPUOwnersIgnoresDevicesWithoutGPUUUID(t *testing.T) {
	g := newGPUOwners()
	shared := []device.Device{
		// The CPU vendor ID is the UUID of a CPU slot.
		{ID: 0, Brand: "cpu", UUID: "GenuineIntel", Type: device.CPU},
		{ID: 1, Brand: "Artificial", UUID: "a0b1", Type: device.CPU},
		{ID: 2, Brand: "zero", Type: device.ZeroSlot},
		{ID: 3, Brand: "test", Type: device.CUDA},
	}
	require.NoError(t, g.claim(&agent{id: "a"}, shared))
	require.NoError(t, g.claim(&agent{id: "b"}, shared))
	require.Empty(t, g.owners)

	rocm := []device.Device{{ID: 0, Brand: "amd", UUID: "0x1234", Type: device.ROCM}}
	require.NoError(t, g.claim(&agent{id: "a"}, rocm))
	require.ErrorContains(t, g.claim(&agent{id: "b"}, rocm), "already reported by agent a")
}

func TestGPUOwnersNil(t *testing.T) {
	var g *gpuOwners
	require.NoError(t, g.claim(&agent{id: aproto.ID("a")}, []device.Device{gpu(0, "GPU-a")}))
	g.release(&agent{id: "a"})
}
