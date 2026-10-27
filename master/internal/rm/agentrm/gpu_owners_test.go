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
	require.Equal(t, map[string][]*agent{"GPU-a": {node01}, "GPU-b": {node01}}, g.owners)

	// The same agent again, as on a reconnect, is accepted.
	require.NoError(t, g.claim(node01, []device.Device{gpu(0, "GPU-a"), gpu(1, "GPU-b")}))

	// So is a new agent with the same ID: it takes the GPUs over, and the old agent's release
	// leaves them with it.
	node01b := &agent{id: "node01"}
	require.NoError(t, g.claim(node01b, []device.Device{gpu(0, "GPU-a")}))
	g.release(node01)
	require.Equal(t, map[string][]*agent{"GPU-a": {node01b}}, g.owners)

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

// Agents restored from snapshots that share a GPU all hold it. A new agent ID is refused while any
// of them does. The first of them to pass its AgentStarted becomes the only holder; the other is
// then refused.
func TestGPUOwnersRestoredAgentsShareGPU(t *testing.T) {
	g := newGPUOwners()
	node01 := &agent{id: "node01"}
	node01new := &agent{id: "node01-new"}
	node09 := &agent{id: "node09"}

	require.NoError(t, g.restore(node01, []device.Device{gpu(0, "GPU-a"), gpu(1, "GPU-b")}))
	require.EqualError(t, g.restore(node01new, []device.Device{gpu(0, "GPU-b"), gpu(1, "GPU-c")}),
		"agent node01-new restored: GPU GPU-b is also restored for agent node01")
	require.Equal(t, map[string][]*agent{
		"GPU-a": {node01}, "GPU-b": {node01, node01new}, "GPU-c": {node01new},
	}, g.owners)

	require.EqualError(t, g.claim(node09, []device.Device{gpu(0, "GPU-c")}),
		"agent node09 refused: GPU GPU-c is already reported by agent node01-new")

	require.NoError(t, g.claim(node01new, []device.Device{gpu(0, "GPU-b"), gpu(1, "GPU-c")}))
	require.Equal(t, map[string][]*agent{
		"GPU-a": {node01}, "GPU-b": {node01new}, "GPU-c": {node01new},
	}, g.owners)
	require.EqualError(t, g.claim(node01, []device.Device{gpu(0, "GPU-a"), gpu(1, "GPU-b")}),
		"agent node01 refused: GPU GPU-b is already reported by agent node01-new")

	// The agent that lost its GPUs times out; its other GPU goes free.
	g.release(node01)
	require.Equal(t, map[string][]*agent{"GPU-b": {node01new}, "GPU-c": {node01new}}, g.owners)
	require.NoError(t, g.claim(node09, []device.Device{gpu(0, "GPU-a")}))
}

// A restored agent that never reconnects releases its GPUs when it is stopped; the other restored
// agent stays a holder, and a new agent ID is accepted only once both are gone.
func TestGPUOwnersRestoredAgentReleased(t *testing.T) {
	g := newGPUOwners()
	first := &agent{id: "first"}
	second := &agent{id: "second"}
	require.NoError(t, g.restore(first, []device.Device{gpu(0, "GPU-a")}))
	require.Error(t, g.restore(second, []device.Device{gpu(0, "GPU-a")}))

	g.release(first)
	require.ErrorContains(t, g.claim(&agent{id: "fresh"}, []device.Device{gpu(0, "GPU-a")}),
		"already reported by agent second")
	g.release(second)
	require.Empty(t, g.owners)
	require.NoError(t, g.claim(&agent{id: "fresh"}, []device.Device{gpu(0, "GPU-a")}))
}

func TestGPUOwnersNil(t *testing.T) {
	var g *gpuOwners
	require.NoError(t, g.claim(&agent{id: aproto.ID("a")}, []device.Device{gpu(0, "GPU-a")}))
	require.NoError(t, g.restore(&agent{id: aproto.ID("a")}, []device.Device{gpu(0, "GPU-a")}))
	g.release(&agent{id: "a"})
}
