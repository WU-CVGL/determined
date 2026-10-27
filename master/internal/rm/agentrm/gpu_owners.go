package agentrm

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// gpuOwners records which registered agents hold each GPU, by GPU UUID. Only CUDA and ROCm devices
// with a UUID count: CPU slots and artificial slots can repeat across agents.
//
// A GPU has one holder, except after a master restart that restored several agents whose snapshots
// share it: each of them holds it until one passes its AgentStarted (claim), which leaves it the
// only holder.
//
// Its lock is taken with an agent's lock held, never the other way around, and it never calls into
// an agent.
type gpuOwners struct {
	mu     sync.Mutex
	owners map[string][]*agent
}

func newGPUOwners() *gpuOwners {
	return &gpuOwners{owners: make(map[string][]*agent)}
}

// claim records a as the only holder of the GPUs in devices. If one of them is held by an agent
// with another ID and not by a, it records nothing and returns an error naming both agents and the
// GPUs. An agent with the same ID is the same node reconnecting or re-registering: a takes its GPUs
// over. So does an agent restored from the snapshot over the other agents restored with the same
// GPUs. A nil gpuOwners accepts every agent.
func (g *gpuOwners) claim(a *agent, devices []device.Device) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	conflicts := map[aproto.ID][]string{}
	for _, uuid := range gpuUUIDs(devices) {
		holders := g.owners[uuid]
		if slices.Contains(holders, a) {
			continue
		}
		for _, h := range holders {
			if h.id != a.id {
				conflicts[h.id] = append(conflicts[h.id], uuid)
			}
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("agent %s refused: %s", a.id,
			describeGPUConflicts(conflicts, "is already reported by agent"))
	}

	for _, uuid := range gpuUUIDs(devices) {
		g.owners[uuid] = []*agent{a}
	}
	return nil
}

// restore records a, restored from the snapshot, as a holder of the GPUs in devices, next to the
// agents restored before it. It returns an error naming the other agents that hold one of them.
func (g *gpuOwners) restore(a *agent, devices []device.Device) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	conflicts := map[aproto.ID][]string{}
	for _, uuid := range gpuUUIDs(devices) {
		for _, h := range g.owners[uuid] {
			if h.id != a.id {
				conflicts[h.id] = append(conflicts[h.id], uuid)
			}
		}
		if !slices.Contains(g.owners[uuid], a) {
			g.owners[uuid] = append(g.owners[uuid], a)
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("agent %s restored: %s", a.id,
			describeGPUConflicts(conflicts, "is also restored for agent"))
	}
	return nil
}

// release forgets a as a holder. GPUs that an agent registered later under the same ID took over
// stay with that agent.
func (g *gpuOwners) release(a *agent) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for uuid, holders := range g.owners {
		holders = slices.DeleteFunc(holders, func(h *agent) bool { return h == a })
		if len(holders) == 0 {
			delete(g.owners, uuid)
		} else {
			g.owners[uuid] = holders
		}
	}
}

func describeGPUConflicts(conflicts map[aproto.ID][]string, verb string) string {
	var ids []aproto.ID
	for id := range conflicts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var parts []string
	for _, id := range ids {
		uuids := conflicts[id]
		sort.Strings(uuids)
		parts = append(parts, fmt.Sprintf("GPU %s %s %s", strings.Join(uuids, ","), verb, id))
	}
	return strings.Join(parts, "; ")
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
