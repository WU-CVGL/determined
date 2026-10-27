package agentrm

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// gpuOwners records which registered agent reports each GPU, by GPU UUID. Only CUDA and ROCm
// devices with a UUID count: CPU slots and artificial slots can repeat across agents.
//
// Its lock is taken with an agent's lock held, never the other way around, and it never calls into
// an agent.
type gpuOwners struct {
	mu     sync.Mutex
	owners map[string]*agent
}

func newGPUOwners() *gpuOwners {
	return &gpuOwners{owners: make(map[string]*agent)}
}

// claim records a as the owner of the GPUs in devices. If an agent with another ID owns one of
// them, it records nothing and returns an error naming both agents and the GPUs. An agent with the
// same ID is the same node reconnecting or re-registering: a takes its GPUs over. A nil gpuOwners
// accepts every agent.
func (g *gpuOwners) claim(a *agent, devices []device.Device) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	conflicts := map[aproto.ID][]string{}
	for _, uuid := range gpuUUIDs(devices) {
		if owner, ok := g.owners[uuid]; ok && owner.id != a.id {
			conflicts[owner.id] = append(conflicts[owner.id], uuid)
		}
	}
	if len(conflicts) > 0 {
		var ids []aproto.ID
		for id := range conflicts {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		var parts []string
		for _, id := range ids {
			uuids := conflicts[id]
			sort.Strings(uuids)
			parts = append(parts, fmt.Sprintf("GPU %s is already reported by agent %s",
				strings.Join(uuids, ","), id))
		}
		return fmt.Errorf("agent %s refused: %s", a.id, strings.Join(parts, "; "))
	}

	for _, uuid := range gpuUUIDs(devices) {
		g.owners[uuid] = a
	}
	return nil
}

// release forgets the GPUs a owns. GPUs that an agent registered later under the same ID took over
// stay with that agent.
func (g *gpuOwners) release(a *agent) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for uuid, owner := range g.owners {
		if owner == a {
			delete(g.owners, uuid)
		}
	}
}

func gpuUUIDs(devices []device.Device) []string {
	var uuids []string
	for _, d := range devices {
		if (d.Type == device.CUDA || d.Type == device.ROCM) && d.UUID != "" {
			uuids = append(uuids, d.UUID)
		}
	}
	return uuids
}
