package agentrm

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/gpuhealth"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// strongSelect runs the selection of a task with prefer_gpu_topology "strong" on a fixture, every
// slot allocatable.
func strongSelect(
	t *testing.T, f topologyFixture, free []int, n int, packNUMA bool, xids map[string]bool,
) gpuChoice {
	t.Helper()
	return selectFreeDevices(selection(free, f.ids, f.build()), n,
		deviceSelection{strong: true, packNUMA: packNUMA, xids: xids})
}

func strongRequest(n int) *sproto.AllocateRequest {
	return &sproto.AllocateRequest{
		AllocationID: "strong", SlotsNeeded: n,
		FittingRequirements: sproto.FittingRequirements{
			SingleAgent: true, GPUTopology: expconf.GPUTopologyStrong,
		},
	}
}

func TestStrongExpectedChoices(t *testing.T) {
	// The node choice holds under every fitting policy and packing switch.
	for _, packNUMA := range []bool{true, false} {
		// GPU 0 has a recent critical XID: NUMA node 0 holds a 2-GPU set only with it, so the set
		// comes from NUMA node 1, although node 0 is the fuller one.
		now := time.Now()
		reader := &gpuXIDReader{}
		reader.set(func() *gpuhealth.XIDSnapshot {
			return &gpuhealth.XIDSnapshot{ByUUID: map[string][]gpuhealth.XID{
				gpuDevice(0).UUID: {{Code: 79, LastObserved: now.Add(-time.Hour)}},
			}}
		})
		xids := reader.recent(now)
		require.Equal(t, map[string]bool{gpuDevice(0).UUID: true}, xids)
		healthy := clusterNode(node02IDs)
		free := []int{0, 1, 4, 5, 6, 7}
		require.Equal(t, []int{4, 5}, deviceIDs(strongSelect(t, healthy, free, 2, packNUMA, xids).devices))
		require.Equal(t, []int{0, 1}, deviceIDs(strongSelect(t, healthy, free, 2, packNUMA, nil).devices))

		// Width: fewer narrow GPUs in the node's best set first.
		for _, c := range []struct {
			name string
			f    topologyFixture
			free []int
			n    int
			want []int
		}{
			{"idle node07", node07, node02IDs, 4, []int{4, 5, 6, 7}},
			{"idle node01", node01Widths, node01IDs, 3, []int{0, 1, 2}},
			{"node05, {0..5} free", node05, intRange(0, 6), 2, []int{0, 2}},
			{"node07, {0,1,4,5,6,7} free", node07, []int{0, 1, 4, 5, 6, 7}, 2, []int{4, 5}},
		} {
			got := strongSelect(t, c.f, c.free, c.n, packNUMA, nil)
			require.Equal(t, c.want, deviceIDs(got.devices), "%s, n=%d, packing %v", c.name, c.n, packNUMA)
		}

		// GPUs in error before width: node07's GPU 1 is narrow; with an NVML error on GPU 5, NUMA
		// node 0's set has no GPU in error and one narrow GPU, node 1's one GPU in error.
		inError := node07
		inError.nvmlError = map[int]bool{5: true}
		require.Equal(t, []int{0, 1, 2, 3}, deviceIDs(strongSelect(t, inError, node02IDs, 4, packNUMA, nil).devices))

		// Then packing's rows 3 and 4: the fullest node that holds the set, then the node with
		// fewer allocatable slots, then the lower node number. The node02 fixture reports no
		// widths: every GPU is of unknown width.
		for _, c := range []struct {
			f    topologyFixture
			free []int
			n    int
			want []int
		}{
			{node02, []int{0, 1, 2, 4, 5, 6, 7}, 2, []int{0, 1}},
			{node02, []int{0, 1, 2, 3, 5, 6, 7}, 2, []int{5, 6}},
			{node01, []int{0, 1, 2, 5, 6, 7}, 3, []int{5, 6, 7}},
			{node02, node02IDs, 2, []int{0, 1}},
			{node02, node02IDs, 4, []int{0, 1, 2, 3}},
			{node01, node01IDs, 4, []int{0, 1, 2, 3}},
		} {
			got := strongSelect(t, c.f, c.free, c.n, packNUMA, nil)
			require.Equal(t, c.want, deviceIDs(got.devices), "free %v, n=%d, packing %v", c.free, c.n, packNUMA)
		}

		// Unknown width ranks between full and narrow.
		f := clusterNode(node02IDs, 1)
		f.width[5] = [2]int{0, 0}
		require.Equal(t, []int{4, 5, 6, 7}, deviceIDs(strongSelect(t, f, node02IDs, 4, packNUMA, nil).devices))
		f = clusterNode(node02IDs)
		f.width[5] = [2]int{0, 0}
		require.Equal(t, []int{0, 1, 2, 3}, deviceIDs(strongSelect(t, f, node02IDs, 4, packNUMA, nil).devices))
	}

	// Plain packing for comparison: it never reads the width.
	require.Equal(t, []int{0, 1, 2, 3}, packed(t, node07, node02IDs, node02IDs, 4))
	require.Equal(t, []int{5, 6, 7}, packed(t, node01Widths, node01IDs, node01IDs, 3))
	require.Equal(t, []int{4, 5}, packed(t, node05, intRange(0, 6), node02IDs, 2))
	require.Equal(t, []int{0, 1}, packed(t, node07, []int{0, 1, 4, 5, 6, 7}, node02IDs, 2))
	require.Equal(t, []int{5, 6, 7}, packed(t, node01, []int{0, 1, 2, 5, 6, 7}, node01IDs, 3))
	healthy := clusterNode(node02IDs)
	free := []int{0, 1, 4, 5, 6, 7}
	require.Equal(t, []int{0, 1}, packed(t, healthy, free, node02IDs, 2))
	c := selectFreeDevices(selection(free, node02IDs, healthy.build()), 2,
		deviceSelection{packNUMA: true, xids: map[string]bool{gpuDevice(0).UUID: true}})
	require.Equal(t, []int{4, 5}, deviceIDs(c.devices))

	// The set and the task log's worst pair.
	c = strongSelect(t, node07, node02IDs, 4, true, nil)
	require.Equal(t, "worst pair NODE, P2P usable", c.worstPair)
	require.Equal(t, "GPU topology preference strong; NUMA node 1; worst pair NODE, P2P usable", c.rule)
	require.Empty(t, c.noSelectionReason)
}

func TestStrongTakesGPUsInErrorWithoutABetterNode(t *testing.T) {
	// GPUs in error rank a set last; they never make a node ineligible.
	f := node02
	f.nvmlError = map[int]bool{1: true}
	for _, packNUMA := range []bool{true, false} {
		c := strongSelect(t, f, []int{0, 1, 2, 3, 4}, 4, packNUMA, nil)
		require.Equal(t, []int{0, 1, 2, 3}, deviceIDs(c.devices))
		require.Equal(t, "GPU topology preference strong; NUMA node 0; worst pair NODE, P2P not usable; "+
			"in error: 1", c.rule)
		require.Equal(t, []int{4, 5, 6, 7}, deviceIDs(strongSelect(t, f, node02IDs, 4, packNUMA, nil).devices))
		// Inside a node, GPUs in error go last.
		require.Equal(t, []int{0, 2, 3}, deviceIDs(strongSelect(t, f, node02IDs, 3, packNUMA, nil).devices))
	}
	// Plain packing takes every healthy GPU with a known NUMA node first.
	require.Equal(t, []int{0, 2, 3, 4}, packed(t, f, []int{0, 1, 2, 3, 4}, node02IDs, 4))

	// The agent fits, and the reservation takes the set.
	state := topologyAgentState(t, f)
	for _, id := range []int{5, 6, 7} {
		cid := cproto.NewID()
		state.Devices[gpuDevice(id)] = &cid
	}
	fits := findFits(strongRequest(4), map[aproto.ID]*agentState{state.id: state}, BestFit, false)
	require.Len(t, fits, 1)
	require.Equal(t, []int{0, 1, 2, 3}, reserve(t, state, 4, deviceSelection{strong: true}))
}

func TestStrongOneNodeSetWhenEveryPairIsUnknown(t *testing.T) {
	// The NUMA nodes are known but no link is: GPUs in error last, then the lowest IDs.
	f := node02
	f.level = func(int, int) aproto.GPULinkLevel { return "" }
	f.p2p = allP2P(p2pUnknown)
	c := strongSelect(t, f, node02IDs, 2, false, nil)
	require.Equal(t, []int{0, 1}, deviceIDs(c.devices))
	require.Equal(t, "worst pair level unknown, P2P unknown", c.worstPair)
	c = strongSelect(t, f, node02IDs, 2, false, map[string]bool{gpuDevice(0).UUID: true})
	require.Equal(t, []int{1, 2}, deviceIDs(c.devices))

	// Above the set cap on one node: the lowest IDs, GPUs in error last.
	ids := intRange(0, 17)
	numa := map[int]int{}
	for _, id := range ids {
		numa[id] = 0
	}
	big := topologyFixture{ids: ids, numa: numa, p2p: allP2P(p2pOK), nvmlError: map[int]bool{2: true}}
	require.Greater(t, binomial(17, 8), maxTopologySets)
	c = strongSelect(t, big, ids, 8, true, nil)
	require.Equal(t, []int{0, 1, 3, 4, 5, 6, 7, 8}, deviceIDs(c.devices))
}

func TestStrongWithoutAOneNodeSetChoosesNothing(t *testing.T) {
	c := strongSelect(t, node02, []int{0, 1, 2, 4, 5, 6}, 4, true, nil)
	require.Nil(t, c.devices)
	require.Equal(t, "no NUMA node has 4 free GPUs", c.noSelectionReason)

	c = selectFreeDevices(selection(node02IDs, node02IDs, nil), 2, deviceSelection{strong: true})
	require.Nil(t, c.devices)
	require.Equal(t, "topology unknown: "+reasonNotReportedSinceMasterStart, c.noSelectionReason)

	// Below 2 slots, strong is as no preference: packing, or map order.
	c = strongSelect(t, node01, node01IDs, 1, true, nil)
	require.Equal(t, []int{5}, deviceIDs(c.devices))
	c = selectFreeDevices(selection(node02IDs, node02IDs, nil), 1, deviceSelection{strong: true})
	require.Nil(t, c.devices)
	require.NotEmpty(t, c.noSelectionReason)
}

func TestStrongFit(t *testing.T) {
	agentsOf := func(states ...*agentState) map[aproto.ID]*agentState {
		out := map[aproto.ID]*agentState{}
		for _, s := range states {
			out[s.id] = s
		}
		return out
	}
	busy := func(state *agentState, ids ...int) *agentState {
		for _, id := range ids {
			cid := cproto.NewID()
			state.Devices[gpuDevice(id)] = &cid
		}
		return state
	}
	plain := func(n int) *sproto.AllocateRequest {
		return &sproto.AllocateRequest{
			AllocationID: "plain", SlotsNeeded: n,
			FittingRequirements: sproto.FittingRequirements{SingleAgent: true},
		}
	}
	fits := func(req *sproto.AllocateRequest, states ...*agentState) bool {
		return len(findFits(req, agentsOf(states...), BestFit, false)) > 0
	}

	idle := topologyAgentState(t, node02)
	require.True(t, fits(strongRequest(4), idle))
	require.False(t, fits(strongRequest(5), idle))
	require.True(t, fits(plain(5), idle))

	// Fewer than 2 slots: as no preference, also without a topology.
	unknown := topologyAgentState(t, node02)
	unknown.gpuTopology = nil
	require.False(t, fits(strongRequest(2), unknown))
	require.True(t, fits(strongRequest(1), unknown))
	require.True(t, fits(plain(2), unknown))
	unknown.gpuTopology = &gpuTopology{unknownReason: "agent 0.40.0 does not report GPU topology"}
	require.False(t, fits(strongRequest(2), unknown))

	// GPUs with an unknown NUMA node never count.
	partial := node02
	partial.numa = map[int]int{0: 0, 1: 0, 2: 0, 4: 1, 5: -1, 6: 1, 7: 1}
	require.False(t, fits(strongRequest(4), topologyAgentState(t, partial)))
	require.True(t, fits(strongRequest(3), topologyAgentState(t, partial)))

	// Draining and disabled slots are not free: 6 free GPUs, 3 on each node.
	state := topologyAgentState(t, node02)
	drainSlot(t, state, 2)
	_, err := state.patchSlotState(patchSlotState{id: 7, enabled: ptrs.Ptr(false)})
	require.NoError(t, err)
	require.Equal(t, 6, state.numEmptySlots())
	require.False(t, fits(strongRequest(4), state))
	require.True(t, fits(strongRequest(3), state))
	require.True(t, fits(plain(6), state))

	// Busy GPUs: 5 free, split 3 and 2.
	require.False(t, fits(strongRequest(4), busy(topologyAgentState(t, node02), 0, 4, 5)))

	// BlockedNodes still hold.
	req := strongRequest(2)
	req.BlockedNodes = []string{string(idle.id)}
	require.False(t, fits(req, idle))

	// CPU slots have no NUMA node.
	cpu := newAgentState("cpu", 100)
	for i := 0; i < 8; i++ {
		cpu.Devices[device.Device{ID: device.ID(i), Type: device.CPU}] = nil
	}
	require.False(t, fits(strongRequest(2), cpu))

	// The agent choice among eligible agents: BestFit prefers a, with 4 empty slots split 2+2, for
	// a plain task, and strong takes b.
	a := busy(topologyAgentState(t, node02), 2, 3, 6, 7)
	a.id = "a"
	b := busy(topologyAgentState(t, node02), 5, 6, 7)
	b.id = "b"
	got := findFits(plain(4), agentsOf(a, b), BestFit, false)
	require.Equal(t, aproto.ID("a"), got[0].Agent.id)
	got = findFits(strongRequest(4), agentsOf(a, b), BestFit, false)
	require.Len(t, got, 1)
	require.Equal(t, aproto.ID("b"), got[0].Agent.id)
}

func TestStrongUsesOneAgent(t *testing.T) {
	// No multi-agent fits: a 16-slot strong task waits on two idle 8-GPU agents.
	agents := map[aproto.ID]*agentState{}
	for _, id := range []aproto.ID{"a", "b"} {
		state := topologyAgentState(t, node02)
		state.id = id
		agents[id] = state
	}
	for _, n := range []int{8, 16} {
		req := strongRequest(n)
		req.FittingRequirements.SingleAgent = false
		require.Empty(t, findFits(req, agents, BestFit, false), "n=%d", n)
		req.FittingRequirements.GPUTopology = expconf.GPUTopologySoft
		require.NotEmpty(t, findFits(req, agents, BestFit, false), "n=%d", n)
	}
}

func TestStrongReservationNeverTakesMapOrder(t *testing.T) {
	strong := deviceSelection{strong: true, packNUMA: true}
	failing := map[string]func(gpuSelectionInput, int, deviceSelection) gpuChoice{
		"across nodes": func(in gpuSelectionInput, _ int, _ deviceSelection) gpuChoice {
			return gpuChoice{devices: gpuDeviceList(0, 4)}
		},
		"unknown NUMA node": func(in gpuSelectionInput, _ int, _ deviceSelection) gpuChoice {
			return gpuChoice{devices: gpuDeviceList(0, 8)}
		},
		"short": func(gpuSelectionInput, int, deviceSelection) gpuChoice { return gpuChoice{devices: gpuDeviceList(0)} },
		"reason": func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			return gpuChoice{noSelectionReason: "injected"}
		},
		"no reason": func(gpuSelectionInput, int, deviceSelection) gpuChoice { return gpuChoice{} },
		"panic":     func(gpuSelectionInput, int, deviceSelection) gpuChoice { panic("injected") },
	}
	for name, selector := range failing {
		f := node02
		f.ids = append(append([]int{}, node02IDs...), 8)
		state := topologyAgentState(t, f)
		before := snapshotOf(state)
		_, err := state.chooseFreeDevices(2, strong, selector)
		require.ErrorContains(t, err, "GPU topology preference strong: ", name)
		require.Equal(t, before, snapshotOf(state), name)
	}

	// No topology, as after a master restart: an error, not map order.
	state := topologyAgentState(t, node02)
	state.gpuTopology = nil
	before := snapshotOf(state)
	_, err := state.allocateFreeDevices(2, cproto.NewID(), strong)
	require.EqualError(t, err, "GPU topology preference strong: topology unknown: "+
		reasonNotReportedSinceMasterStart)
	require.Equal(t, before, snapshotOf(state))

	// No NUMA node holds the set.
	state = topologyAgentState(t, node02)
	for _, id := range []int{0, 4, 5} {
		cid := cproto.NewID()
		state.Devices[gpuDevice(id)] = &cid
	}
	before = snapshotOf(state)
	_, err = state.chooseFreeDevices(4, strong, selectFreeDevices)
	require.EqualError(t, err, "GPU topology preference strong: no NUMA node has 4 free GPUs")
	require.Equal(t, before, snapshotOf(state))

	// The reservation of a valid set: its choice says how, and nothing failed.
	state = topologyAgentState(t, node07)
	res, err := state.allocateFreeDevices(4, cproto.NewID(), strong)
	require.NoError(t, err)
	require.Empty(t, res.failure)
	require.Equal(t, []int{4, 5, 6, 7}, deviceIDs(res.devices))
	require.Equal(t, "worst pair NODE, P2P usable", res.choice.worstPair)
}

func TestStrongMissInTheSimulationIsNotAPanic(t *testing.T) {
	// The agent changed after the fit: GPUs 0 and 4 became busy, so 6 GPUs are free but no NUMA
	// node holds 4. The simulation counts a miss and leaves its copies as they were.
	copies := map[aproto.ID]*agentState{"a": topologyAgentState(t, node02)}
	copies["a"].id = "a"
	req := strongRequest(4)
	p := priorityScheduler{gpus: gpuPolicy{packNUMA: true}}
	fits := findFits(req, copies, BestFit, false)
	require.Len(t, fits, 1)
	for _, id := range []int{0, 4} {
		cid := cproto.NewID()
		copies["a"].Devices[gpuDevice(id)] = &cid
	}
	before := snapshotOf(copies["a"])
	require.NotPanics(t, func() { require.False(t, p.addTaskToAgents(req, fits)) })
	require.Equal(t, before, snapshotOf(copies["a"]))
}

func TestStrongCannotFit(t *testing.T) {
	agentsOf := func(states ...*agentState) map[aproto.ID]*agentState {
		out := map[aproto.ID]*agentState{}
		for i, s := range states {
			s.id = aproto.ID(fmt.Sprintf("agent-%d", i))
			out[s.id] = s
		}
		return out
	}
	require.NoError(t, strongCannotFit("p", nil, 4), "no agent: wait")

	node := topologyAgentState(t, node02)
	require.NoError(t, strongCannotFit("p", agentsOf(node), 4))
	require.EqualError(t, strongCannotFit("p", agentsOf(node), 5), "no NUMA node in pool p has 5 slots; use soft")

	// Every slot counts, also disabled and busy ones.
	for _, id := range []int{0, 1, 2} {
		_, err := node.patchSlotState(patchSlotState{id: device.ID(id), enabled: ptrs.Ptr(false)})
		require.NoError(t, err)
	}
	cid := cproto.NewID()
	node.Devices[gpuDevice(3)] = &cid
	require.NoError(t, strongCannotFit("p", agentsOf(node), 4))

	// exclude_gpus leaves node01 with 3 slots on NUMA node 1.
	require.NoError(t, strongCannotFit("p", agentsOf(topologyAgentState(t, node01)), 4))
	require.Error(t, strongCannotFit("p", agentsOf(topologyAgentState(t, node01)), 5))

	// An agent that has not reported since the master started: wait.
	restored := topologyAgentState(t, node02)
	restored.gpuTopology = nil
	require.NoError(t, strongCannotFit("p", agentsOf(topologyAgentState(t, node02), restored), 5))

	// No known NUMA node: CPU slots, a topology with an unknown reason.
	cpu := newAgentState("cpu", 100)
	cpu.gpuTopology = &gpuTopology{}
	for i := 0; i < 8; i++ {
		cpu.Devices[device.Device{ID: device.ID(i), Type: device.CPU}] = nil
	}
	old := topologyAgentState(t, node02)
	old.gpuTopology = &gpuTopology{unknownReason: "agent 0.40.0 does not report GPU topology"}
	require.EqualError(t, strongCannotFit("p", agentsOf(cpu, old), 2),
		"no agent in pool p reports NUMA nodes; use soft")
}

func TestGPUPolicySelectionStrong(t *testing.T) {
	policy := gpuPolicy{packNUMA: true}
	one := []*fittingState{{Slots: 4}}
	for n, want := range map[int]bool{0: false, 1: false, 2: true, 4: true} {
		req := strongRequest(n)
		sel := policy.selection(req, one)
		require.Equal(t, want, sel.strong, "n=%d", n)
		require.False(t, sel.preferTopology, "n=%d", n)
		require.True(t, sel.packNUMA)
	}
	req := strongRequest(4)
	req.FittingRequirements.GPUTopology = expconf.GPUTopologySoft
	require.False(t, policy.selection(req, one).strong)
}

// bruteForceStrong returns strong's set by its definition: for each NUMA node with n free GPUs of
// a known NUMA node, the node's best set, enumerated (GPUs in error, then the pair ranks worst
// first when a free pair of the node is rankable and the node has at most maxTopologySets sets,
// then the lowest IDs); then the node with the smallest key (GPUs in error in its best set, narrow
// GPUs, GPUs of unknown width, free healthy GPUs on the node, allocatable healthy slots on the
// node, node number).
func bruteForceStrong(g *gpuTopology, free, allocatable []int, n int, xids map[string]bool) []int {
	byNode := map[int][]int{}
	healthyFree, healthySlots := map[int]int{}, map[int]int{}
	faulty := func(id int) bool { return gpuInError(g, gpuDevice(id), xids) }
	for _, id := range free {
		if node := numaNodeOf(g, gpuDevice(id)); node >= 0 {
			byNode[node] = append(byNode[node], id)
			if !faulty(id) {
				healthyFree[node]++
			}
		}
	}
	for _, id := range allocatable {
		if node := numaNodeOf(g, gpuDevice(id)); node >= 0 && !faulty(id) {
			healthySlots[node]++
		}
	}

	var best []int
	var bestKey [6]int
	for node, ids := range byNode {
		if len(ids) < n {
			continue
		}
		rankable := false
		for i, a := range ids {
			for _, b := range ids[i+1:] {
				if !g.pairRank(device.ID(a), device.ID(b)).linkUnknown() {
					rankable = true
				}
			}
		}
		rankable = rankable && binomial(len(ids), n) <= maxTopologySets
		var nodeBest []int
		var nodeFaulty int
		var nodePairs []pairRank
		forEachSubset(len(ids), n, func(idx []int) {
			set := make([]int, n)
			setFaulty := 0
			var pairs []pairRank
			for a, i := range idx {
				set[a] = ids[i]
				if faulty(ids[i]) {
					setFaulty++
				}
				for _, j := range idx[a+1:] {
					pairs = append(pairs, g.pairRank(device.ID(ids[i]), device.ID(ids[j])))
				}
			}
			sort.Slice(pairs, func(x, y int) bool { return comparePairRanks(pairs[x], pairs[y]) > 0 })
			cmp := -1
			if nodeBest != nil {
				cmp = cmpInt(setFaulty, nodeFaulty)
				for x := 0; rankable && cmp == 0 && x < len(pairs); x++ {
					cmp = comparePairRanks(pairs[x], nodePairs[x])
				}
				// Exact ties keep the first set, which has the lowest IDs.
			}
			if cmp < 0 {
				nodeBest, nodeFaulty, nodePairs = set, setFaulty, pairs
			}
		})
		key := [6]int{nodeFaulty, 0, 0, healthyFree[node], healthySlots[node], node}
		for _, id := range nodeBest {
			switch g.gpuWidth(device.ID(id)) {
			case widthNarrow:
				key[1]++
			case widthUnknown:
				key[2]++
			}
		}
		if best == nil || lexLess(key, bestKey) {
			best, bestKey = nodeBest, key
		}
	}
	return best
}

// lexLess compares the rows of two node keys in order, independently of compareStrongNodeKeys.
func lexLess(a, b [6]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func TestStrongEqualsBruteForce(t *testing.T) {
	// Over random topologies (levels, P2P, NVLinks, link widths, unknown NUMA nodes, NVML errors and
	// XIDs) and random free and allocatable slots, strong's set is the argmin of its node rule, its
	// fit admits exactly the agents where it chooses a set, and that set lies on one NUMA node.
	rng := rand.New(rand.NewSource(11)) //nolint:gosec
	levels := []aproto.GPULinkLevel{
		aproto.GPULinkLevelPIX, aproto.GPULinkLevelPXB, aproto.GPULinkLevelNode, aproto.GPULinkLevelSys, "",
	}
	caps := []aproto.GPUP2PCaps{p2pOK, p2pNotOK, p2pUnknown}
	widths := [][2]int{x16, x16, x8, {0, 16}}
	cases, none := 0, 0
	for trial := 0; trial < 2000; trial++ {
		m := 2 + rng.Intn(9)
		nodes := 1 + rng.Intn(3)
		f := topologyFixture{
			ids: intRange(0, m), numa: map[int]int{}, nvmlError: map[int]bool{}, width: map[int][2]int{},
		}
		xids := map[string]bool{}
		for id := 0; id < m; id++ {
			if rng.Float64() >= 0.15 {
				f.numa[id] = rng.Intn(nodes)
			}
			f.nvmlError[id] = rng.Float64() < 0.1
			if rng.Float64() < 0.1 {
				xids[gpuDevice(id).UUID] = true
			}
			f.width[id] = widths[rng.Intn(len(widths))]
		}
		g := f.build()
		allLinksUnknown := rng.Float64() < 0.15
		for k := range g.pairs {
			p2p := caps[rng.Intn(len(caps))]
			pair := gpuPair{level: levels[rng.Intn(len(levels))], p2pAToB: p2p, p2pBToA: p2p}
			if rng.Float64() < 0.2 {
				pair.nvlinks = 1 + rng.Intn(2)
			}
			if allLinksUnknown {
				pair = gpuPair{}
			}
			g.pairs[k] = pair
		}
		var free, allocatable []int
		for id := 0; id < m; id++ {
			isFree := rng.Float64() < 0.75
			if isFree {
				free = append(free, id)
			}
			if isFree || rng.Float64() < 0.8 {
				allocatable = append(allocatable, id)
			}
		}
		in := selection(free, allocatable, g)
		for n := 2; n <= len(free); n++ {
			for _, packNUMA := range []bool{false, true} {
				c := selectFreeDevices(in, n, deviceSelection{strong: true, packNUMA: packNUMA, xids: xids})
				want := bruteForceStrong(g, free, allocatable, n, xids)
				var got []int
				if c.devices != nil {
					got = deviceIDs(c.devices)
				}
				require.Equal(t, want, got,
					"trial %d, numa %v, free %v, allocatable %v, n=%d, packing %v",
					trial, f.numa, free, allocatable, n, packNUMA)
				require.Equal(t, want != nil, holdsOnOneNUMANode(in, n))
				if want == nil {
					none++
					require.NotEmpty(t, c.noSelectionReason)
					continue
				}
				cases++
				node := numaNodeOf(g, c.devices[0])
				for _, d := range c.devices {
					require.Equal(t, node, numaNodeOf(g, d))
				}
			}
		}
	}
	t.Logf("strong equals the brute-force argmin in %d cases; %d without a one-node set", cases, none)
}

func TestStrongReservationOverRandomStates(t *testing.T) {
	// Over random agent states with busy, draining and disabled slots, the fit and the reservation
	// agree: a strong reservation succeeds exactly on an agent its fit admits, on one NUMA node,
	// and changes nothing when it fails.
	rng := rand.New(rand.NewSource(13)) //nolint:gosec
	for trial := 0; trial < 500; trial++ {
		state := topologyAgentState(t, node01Widths)
		for _, id := range node01IDs {
			d := gpuDevice(id)
			switch rng.Intn(6) {
			case 0:
				cid := cproto.NewID()
				state.Devices[d] = &cid
			case 1:
				cid := cproto.NewID()
				state.Devices[d] = &cid
				drainSlot(t, state, d.ID)
			case 2:
				_, err := state.patchSlotState(patchSlotState{id: d.ID, enabled: ptrs.Ptr(false)})
				require.NoError(t, err)
			case 3:
				drainSlot(t, state, d.ID)
			}
		}
		n := 2 + rng.Intn(4)
		fit := len(findFits(strongRequest(n), map[aproto.ID]*agentState{state.id: state}, BestFit, false)) > 0
		before := snapshotOf(state)
		res, err := state.allocateFreeDevices(n, cproto.NewID(), deviceSelection{strong: true, packNUMA: true})
		require.Equal(t, fit, err == nil, "trial %d, n=%d: %v", trial, n, err)
		if err != nil {
			require.Equal(t, before, snapshotOf(state))
			continue
		}
		require.NoError(t, state.checkOneNUMANode(res.devices))
		for _, d := range res.devices {
			require.True(t, state.slotStates[d.ID].enabled.allocatable(), "slot %d", d.ID)
		}
	}
}
