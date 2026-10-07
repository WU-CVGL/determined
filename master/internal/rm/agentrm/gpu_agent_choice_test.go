package agentrm

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// The agent choice of prefer_gpu_topology "soft": under the pool's gate (fitting_policy best,
// numa_packing on), a soft request with 2 or more slots on one agent takes an agent where one NUMA
// node has its slots free before one that would split it. The fixtures have GPUs 0-3 on NUMA node
// 0 and 4-7 on node 1.

// fitsUnder is findFits with the pool's gate.
func fitsUnder(
	req *sproto.AllocateRequest, agents map[aproto.ID]*agentState, fitting SoftConstraint, gate bool,
) []*fittingState {
	return findFits(req, agents, fitting, false, gate)
}

// choiceAgent returns an agent of a fixture with these GPUs busy.
func choiceAgent(t *testing.T, id aproto.ID, f topologyFixture, busy ...int) *agentState {
	t.Helper()
	state := topologyAgentState(t, f)
	state.id = id
	for _, b := range busy {
		cid := cproto.NewID()
		state.Devices[gpuDevice(b)] = &cid
	}
	return state
}

func choiceAgents(states ...*agentState) map[aproto.ID]*agentState {
	out := map[aproto.ID]*agentState{}
	for _, s := range states {
		out[s.id] = s
	}
	return out
}

func topologyRequest(id string, n int, pref expconf.GPUTopologyPreference) *sproto.AllocateRequest {
	return &sproto.AllocateRequest{
		AllocationID: model.AllocationID(id), SlotsNeeded: n,
		FittingRequirements: sproto.FittingRequirements{SingleAgent: true, GPUTopology: pref},
	}
}

// agentUnder returns the agent of a request's single-agent fit, or "" without a fit.
func agentUnder(
	t *testing.T, req *sproto.AllocateRequest, agents map[aproto.ID]*agentState, fitting SoftConstraint,
	gate bool,
) aproto.ID {
	t.Helper()
	fits := fitsUnder(req, agents, fitting, gate)
	if len(fits) == 0 {
		return ""
	}
	require.Len(t, fits, 1, "request %s", req.AllocationID)
	return fits[0].Agent.id
}

const soft, strong = expconf.GPUTopologySoft, expconf.GPUTopologyStrong

func TestSoftPrefersAnAgentWithAOneNodeBlock(t *testing.T) {
	// a is fuller and would split a 4-GPU task (free 0,1 | 4,5); b has 5 free, 4 of them on NUMA
	// node 0. Under the gate, soft and strong take b, a plain task takes a. With the gate off
	// (numa_packing false), soft takes a, as a plain task does.
	agents := choiceAgents(
		choiceAgent(t, "a", node02, 2, 3, 6, 7),
		choiceAgent(t, "b", node02, 5, 6, 7),
	)
	require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 4, ""), agents, BestFit, true))
	require.Equal(t, aproto.ID("b"), agentUnder(t, topologyRequest("r", 4, soft), agents, BestFit, true))
	require.Equal(t, aproto.ID("b"), agentUnder(t, topologyRequest("r", 4, strong), agents, BestFit, true))
	require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 4, soft), agents, BestFit, false))

	// 3 GPUs: only b holds them on one node. 2 GPUs: both do, and BestFit takes a.
	require.Equal(t, aproto.ID("b"), agentUnder(t, topologyRequest("r", 3, soft), agents, BestFit, true))
	require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 3, ""), agents, BestFit, true))
	require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 2, soft), agents, BestFit, true))

	// GPUs in error count, as for strong: the fit reads no health.
	inError := node02
	inError.nvmlError = map[int]bool{0: true, 1: true}
	agents["b"] = choiceAgent(t, "b", inError, 5, 6, 7)
	require.Equal(t, aproto.ID("b"), agentUnder(t, topologyRequest("r", 4, soft), agents, BestFit, true))
}

func TestSoftAgentChoiceWithEqualCounts(t *testing.T) {
	// Both agents with a one-node block: BestFit takes the fuller one.
	agents := choiceAgents(
		choiceAgent(t, "a", node02, 0, 1, 2),
		choiceAgent(t, "b", node02),
	)
	require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 4, soft), agents, BestFit, true))
	require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 4, ""), agents, BestFit, true))

	// Equal free counts, so equal scores: a is split 3+3, b has 4 free on NUMA node 1. Soft takes b
	// for every task; for a plain task, and for soft with the gate off, the hash distance decides,
	// and both agents occur.
	agents = choiceAgents(
		choiceAgent(t, "a", node02, 0, 4),
		choiceAgent(t, "b", node02, 0, 1),
	)
	plainAgents := map[aproto.ID]bool{}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("task-%d", i)
		require.Equal(t, aproto.ID("b"), agentUnder(t, topologyRequest(id, 4, soft), agents, BestFit, true), id)
		plain := agentUnder(t, topologyRequest(id, 4, ""), agents, BestFit, true)
		require.Equal(t, plain, agentUnder(t, topologyRequest(id, 4, soft), agents, BestFit, false), id)
		plainAgents[plain] = true
	}
	require.Len(t, plainAgents, 2)
}

func TestSoftAgentChoiceWithUnknownTopology(t *testing.T) {
	unknownTopologies := map[string]*gpuTopology{
		"not reported": nil,
		"unknown":      {unknownReason: "agent 0.40.0 does not report GPU topology"},
	}
	for name, unknown := range unknownTopologies {
		// a's topology is unknown: it is with the agents that would split the task. b, emptier,
		// has a one-node block, so soft takes b.
		a := choiceAgent(t, "a", node02, 5, 6, 7)
		a.gpuTopology = unknown
		agents := choiceAgents(a, choiceAgent(t, "b", node02, 0))
		require.Equal(t, aproto.ID("b"), agentUnder(t, topologyRequest("r", 4, soft), agents, BestFit, true), name)
		require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 4, ""), agents, BestFit, true), name)

		// b would split the task too: BestFit takes a.
		agents = choiceAgents(a, choiceAgent(t, "b", node02, 0, 4))
		require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 4, soft), agents, BestFit, true), name)

		// Every agent unknown: soft takes the agent a plain task takes.
		b := choiceAgent(t, "b", node02, 0, 1, 2)
		b.gpuTopology = unknown
		c := choiceAgent(t, "c", node02, 4, 5)
		c.gpuTopology = unknown
		agents = choiceAgents(a, b, c)
		for i := 0; i < 20; i++ {
			for _, n := range []int{2, 3, 4} {
				id := fmt.Sprintf("task-%d", i)
				require.Equal(t,
					agentUnder(t, topologyRequest(id, n, ""), agents, BestFit, true),
					agentUnder(t, topologyRequest(id, n, soft), agents, BestFit, true), "%s, %s, n=%d", name, id, n)
			}
		}
	}
}

func TestSoftAgentChoiceUnchanged(t *testing.T) {
	// n = 1: BestFit takes a, the fuller agent, although its topology is unknown and b has a free
	// GPU with a known NUMA node.
	a := choiceAgent(t, "a", node02, 1, 2, 3, 4, 5, 6, 7)
	a.gpuTopology = nil
	agents := choiceAgents(a, choiceAgent(t, "b", node02))
	require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 1, soft), agents, BestFit, true))

	// 5 or more GPUs on 4+4 agents: no NUMA node holds them, so soft takes the agent a plain task
	// takes.
	agents = choiceAgents(choiceAgent(t, "a", node02, 0, 1), choiceAgent(t, "b", node02))
	for n, want := range map[int]aproto.ID{5: "a", 6: "a", 7: "b", 8: "b"} {
		require.Equal(t, want, agentUnder(t, topologyRequest("r", n, soft), agents, BestFit, true), "n=%d", n)
		require.Equal(t, want, agentUnder(t, topologyRequest("r", n, ""), agents, BestFit, true), "n=%d", n)
	}

	// Under worst the gate is off: soft takes the agent WorstFit picks, a (6 free, split 3+3), not
	// b (5 free, 4 of them on NUMA node 1).
	agents = choiceAgents(choiceAgent(t, "a", node02, 0, 4), choiceAgent(t, "b", node02, 0, 1, 2))
	require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 4, soft), agents, WorstFit, false))
	require.Equal(t, aproto.ID("a"), agentUnder(t, topologyRequest("r", 4, ""), agents, WorstFit, false))

	// Zero slots: the hash spread, as for a plain task.
	agents = choiceAgents(choiceAgent(t, "a", node02), choiceAgent(t, "b", node02), choiceAgent(t, "c", node02))
	spread := map[aproto.ID]bool{}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("task-%d", i)
		got := agentUnder(t, topologyRequest(id, 0, soft), agents, BestFit, true)
		require.Equal(t, agentUnder(t, topologyRequest(id, 0, ""), agents, BestFit, false), got, id)
		spread[got] = true
	}
	require.Greater(t, len(spread), 1)

	// Whole agents: a soft 16-GPU task takes both idle agents.
	agents = choiceAgents(choiceAgent(t, "a", node02), choiceAgent(t, "b", node02, 0))
	req := topologyRequest("r", 16, soft)
	req.FittingRequirements.SingleAgent = false
	require.Empty(t, fitsUnder(req, agents, BestFit, true))
	agents["b"] = choiceAgent(t, "b", node02)
	require.Len(t, fitsUnder(req, agents, BestFit, true), 2)

	// Strong: the same agent with the gate on or off, over random states.
	rng := rand.New(rand.NewSource(5)) //nolint:gosec
	for trial := 0; trial < 200; trial++ {
		agents := map[aproto.ID]*agentState{}
		for i := 0; i < 3; i++ {
			var busy []int
			for _, id := range node02IDs {
				if rng.Intn(2) == 0 {
					busy = append(busy, id)
				}
			}
			state := choiceAgent(t, aproto.ID(fmt.Sprintf("agent-%d", i)), node02, busy...)
			agents[state.id] = state
		}
		for n := 2; n <= 4; n++ {
			req := topologyRequest(fmt.Sprintf("task-%d", trial), n, strong)
			require.Equal(t, agentUnder(t, req, agents, BestFit, false), agentUnder(t, req, agents, BestFit, true))
		}
	}
}

// clusterPlan is the check after deploy on the pool of node03 and node04, both of the cluster's
// layout: with node03's slots 0, 1 and 4 and node04's slot 0 disabled, node03 has 5 free GPUs,
// split 2+3, and node04 7, with 4 on NUMA node 1.
func clusterPlan(t *testing.T, policy gpuPolicy, tasks ...strongTask) *strongPool {
	t.Helper()
	layout := clusterNode(node02IDs)
	s := newStrongPool(t, false, policy, map[aproto.ID]topologyFixture{"node03": layout, "node04": layout}, tasks...)
	for agent, slots := range map[aproto.ID][]device.ID{"node03": {0, 1, 4}, "node04": {0}} {
		for _, id := range slots {
			_, err := s.live[agent].patchSlotState(patchSlotState{id: id, enabled: ptrs.Ptr(false)})
			require.NoError(t, err)
		}
	}
	return s
}

// placed returns the agent and the GPUs of an allocated task.
func (s *strongPool) placed(t *testing.T, id string) (aproto.ID, []int) {
	t.Helper()
	allocated := s.rp.taskList.Allocation(model.AllocationID(id))
	require.NotNil(t, allocated, id)
	require.Len(t, allocated.Resources, 1)
	for _, r := range allocated.Resources {
		c := r.(*containerResources)
		return c.agent.id, deviceIDs(c.devices)
	}
	return "", nil
}

func TestSoftAgentChoiceInAPass(t *testing.T) {
	// The check after deploy in miniature, through scheduling passes: under best with packing, a
	// plain 4-GPU task gets node03 {2,5,6,7}; soft and strong get node04 {4,5,6,7}. With packing
	// off, soft gets node03, as a plain task does, and still its best set there.
	type want struct {
		agent aproto.ID
		gpus  []int
	}
	for _, c := range []struct {
		policy gpuPolicy
		mode   expconf.GPUTopologyPreference
		want   want
	}{
		{gpuPolicy{packNUMA: true}, "", want{"node03", []int{2, 5, 6, 7}}},
		{gpuPolicy{packNUMA: true}, soft, want{"node04", []int{4, 5, 6, 7}}},
		{gpuPolicy{packNUMA: true}, strong, want{"node04", []int{4, 5, 6, 7}}},
		{gpuPolicy{}, soft, want{"node03", []int{2, 5, 6, 7}}},
		{gpuPolicy{}, strong, want{"node04", []int{4, 5, 6, 7}}},
	} {
		s := clusterPlan(t, c.policy, strongTask{id: "four", slots: 4, priority: 42, strong: c.mode == strong})
		req, ok := s.rp.taskList.TaskByID("four")
		require.True(t, ok)
		req.FittingRequirements.GPUTopology = c.mode
		allocated, _, again := s.pass(t)
		require.Contains(t, allocated, "four", "%+v", c)
		require.False(t, again)
		agent, gpus := s.placed(t, "four")
		require.Equal(t, c.want, want{agent, gpus}, "mode %q, packing %v", c.mode, c.policy.packNUMA)
	}

	// Without packing, a plain task also gets node03.
	s := clusterPlan(t, gpuPolicy{}, strongTask{id: "four", slots: 4, priority: 42})
	allocated, _, _ := s.pass(t)
	require.Contains(t, allocated, "four")
	agent, _ := s.placed(t, "four")
	require.Equal(t, aproto.ID("node03"), agent)

	// The pass plans its later tasks where its simulation put the soft task. With packing, soft is
	// on node04, so a 6-GPU task at the same priority fits on neither agent (node03 has 5 free,
	// node04 3) and a 1-GPU task at a lower priority waits behind it. Without packing, soft is on
	// node03, and both start.
	for _, c := range []struct {
		policy gpuPolicy
		want   []string
	}{
		{gpuPolicy{packNUMA: true}, []string{"four"}},
		{gpuPolicy{}, []string{"four", "six", "one"}},
	} {
		s := clusterPlan(t, c.policy,
			strongTask{id: "four", slots: 4, priority: 42},
			strongTask{id: "six", slots: 6, priority: 42},
			strongTask{id: "one", slots: 1, priority: 50},
		)
		req, ok := s.rp.taskList.TaskByID("four")
		require.True(t, ok)
		req.FittingRequirements.GPUTopology = soft
		allocated, _, _ := s.pass(t)
		var got []string
		for id := range allocated {
			got = append(got, id)
		}
		require.ElementsMatch(t, c.want, got, "packing %v", c.policy.packNUMA)
	}
}

func TestFairShareRequestsUnchangedByAgentPreference(t *testing.T) {
	// Schedule()'s output for the same snapshot, the requests to allocate, is the same with the gate
	// on and off: the fair-share scheduler only checks whether a request fits, so its demand, quotas
	// and request list do not change. Every request is pending, so the empty release list is not a
	// check of release behaviour. The reservations are not checked: they run one by one, and the
	// agent choice changes the free counts that later requests see, so which requests actually
	// start can change. With soft 4 then plain 6 on agents with 2+3 and 3+4 free, the preference
	// puts the soft task on the 3+4 agent, and the plain 6 no longer fits.
	for trial := 0; trial < 50; trial++ {
		rng := rand.New(rand.NewSource(int64(trial))) //nolint:gosec
		var tasks []*MockTask
		var groups []*MockGroup
		modes := map[model.AllocationID]expconf.GPUTopologyPreference{}
		for i := 0; i < 6; i++ {
			group := &MockGroup{ID: fmt.Sprintf("job-%d", i%3), Weight: 1}
			if i < 3 {
				groups = append(groups, group)
			}
			id := model.AllocationID(fmt.Sprintf("task-%d", i))
			tasks = append(tasks, &MockTask{ID: id, SlotsNeeded: 1 + rng.Intn(5), Group: groups[i%3]})
			modes[id] = []expconf.GPUTopologyPreference{"", soft, strong}[rng.Intn(3)]
		}
		var outcomes [2][]model.AllocationID
		for g, gate := range []bool{false, true} {
			rp := setupResourcePool(t, nil, nil, tasks, groups, nil)
			t.Cleanup(rp.stop)
			for id, mode := range modes {
				req, ok := rp.taskList.TaskByID(id)
				require.True(t, ok)
				req.FittingRequirements.GPUTopology = mode
			}
			r := rand.New(rand.NewSource(int64(trial))) //nolint:gosec
			agents := map[aproto.ID]*agentState{}
			for i := 0; i < 2; i++ {
				var busy []int
				for _, id := range node02IDs {
					if r.Intn(3) == 0 {
						busy = append(busy, id)
					}
				}
				state := choiceAgent(t, aproto.ID(fmt.Sprintf("agent-%d", i)), node02, busy...)
				agents[state.id] = state
			}
			rp.mu.Lock()
			rp.agentStatesCache = agents
			rp.gpuPolicy = gpuPolicy{packNUMA: gate}
			toAllocate, toRelease := rp.scheduler.Schedule(rp)
			rp.mu.Unlock()
			require.Empty(t, toRelease, "trial %d", trial)
			for _, req := range toAllocate {
				outcomes[g] = append(outcomes[g], req.AllocationID)
			}
		}
		require.ElementsMatch(t, outcomes[0], outcomes[1], "trial %d", trial)
	}
}

func TestAgentChoicePlanMatchesTheReservations(t *testing.T) {
	// Over random agent states (busy, disabled and draining slots, unknown topologies, XIDs) and
	// random mixes of soft, plain and strong tasks at two priorities, under best with packing on and
	// no preemption: the scheduler's own simulation step (trySchedulingPendingTasksInPriority), run
	// for each request the pass plans, in the order the pass returns them, on copies of the agents
	// with the pass's policy, chooses the agents and the GPUs that the live reservation of that
	// request then takes, and the agents end as the copies do. Without preemption, the requests the
	// pass returns are the first it places on its copies, in that order.
	fixtures := []topologyFixture{node02, node01, node07, node05, clusterNode(node02IDs)}
	moved, softPlanned, planned := 0, 0, 0
	for trial := 0; trial < 500; trial++ {
		rng := rand.New(rand.NewSource(int64(trial))) //nolint:gosec
		agents := map[aproto.ID]topologyFixture{}
		for i := 0; i < 2+rng.Intn(2); i++ {
			agents[aproto.ID(fmt.Sprintf("agent-%d", i))] = fixtures[rng.Intn(len(fixtures))]
		}
		var tasks []strongTask
		modes := map[string]expconf.GPUTopologyPreference{}
		for i := 0; i < 3+rng.Intn(6); i++ {
			id := fmt.Sprintf("task-%d", i)
			mode := []expconf.GPUTopologyPreference{"", soft, soft, strong}[rng.Intn(4)]
			n := []int{1, 2, 2, 3, 4, 4, 5, 6, 8, 16}[rng.Intn(10)]
			modes[id] = mode
			tasks = append(tasks, strongTask{
				id: id, slots: n, priority: 10 + 32*rng.Intn(2), strong: mode == strong,
			})
		}
		var xids map[string]bool
		if rng.Intn(3) == 0 {
			xids = map[string]bool{gpuDevice(rng.Intn(8)).UUID: true}
		}
		policy := gpuPolicy{packNUMA: true, xids: xids}
		s := newStrongPool(t, false, policy, agents, tasks...)
		for _, task := range tasks {
			req, ok := s.rp.taskList.TaskByID(model.AllocationID(task.id))
			require.True(t, ok)
			req.FittingRequirements.GPUTopology = modes[task.id]
			// Whole-agent fits too, for some tasks of 8 or more slots.
			if req.SlotsNeeded >= 8 && rng.Intn(2) == 0 {
				req.FittingRequirements.SingleAgent = false
			}
		}
		for i := 0; i < len(agents); i++ {
			state := s.live[aproto.ID(fmt.Sprintf("agent-%d", i))]
			switch rng.Intn(6) {
			case 0:
				state.gpuTopology = nil
			case 1:
				state.gpuTopology = &gpuTopology{unknownReason: "agent 0.40.0 does not report GPU topology"}
			}
			for _, id := range freeDeviceIDs(state) {
				d := gpuDevice(int(id))
				switch rng.Intn(12) {
				case 0, 1: // busy
					cid := cproto.NewID()
					state.Devices[d] = &cid
				case 2: // busy and draining
					cid := cproto.NewID()
					state.Devices[d] = &cid
					drainSlot(t, state, d.ID)
				case 3: // disabled
					_, err := state.patchSlotState(patchSlotState{id: d.ID, enabled: ptrs.Ptr(false)})
					require.NoError(t, err)
				}
			}
		}

		s.rp.mu.Lock()
		s.rp.agentStatesCache = s.live
		s.rp.gpuPolicy = policy
		toAllocate, toRelease := s.rp.scheduler.Schedule(s.rp)
		s.rp.mu.Unlock()
		require.Empty(t, toRelease)

		// The pass's scheduler with the pass's policy, as Schedule sets it.
		simulation := *s.rp.scheduler.(*priorityScheduler)
		simulation.gpus = policy
		copies := deepCopyAgents(s.live)
		planned += len(toAllocate)
		for _, req := range toAllocate {
			before := map[aproto.ID][]device.ID{}
			for id, state := range copies {
				before[id] = freeDeviceIDs(state)
			}
			placed, _ := simulation.trySchedulingPendingTasksInPriority(
				[]*sproto.AllocateRequest{req}, copies, s.rp.fittingMethod,
			)
			require.Len(t, placed, 1, "trial %d, request %s", trial, req.AllocationID)
			chosen := map[aproto.ID][]int{}
			for id, state := range copies {
				if taken := idsMinus(before[id], freeDeviceIDs(state)); len(taken) > 0 {
					chosen[id] = taken
				}
			}

			// The live reservation, as allocateResources makes it.
			fits := fitsUnder(req, s.live, s.rp.fittingMethod, policy.packNUMA)
			if len(fits) == 1 && preferOneNUMANode(req, policy.packNUMA) {
				softPlanned++
				if plain := fitsUnder(req, s.live, s.rp.fittingMethod, false); plain[0].Agent.id != fits[0].Agent.id {
					moved++
				}
			}
			sel := policy.selection(req, fits)
			reserved := map[aproto.ID][]int{}
			for _, fit := range fits {
				res, err := fit.Agent.allocateFreeDevices(fit.Slots, cproto.NewID(), sel)
				require.NoError(t, err, "trial %d, request %s", trial, req.AllocationID)
				require.Empty(t, res.failure)
				reserved[fit.Agent.id] = deviceIDs(res.devices)
			}
			require.Equal(t, chosen, reserved, "trial %d, request %s", trial, req.AllocationID)
		}
		for id, state := range s.live {
			require.Equal(t, freeDeviceIDs(copies[id]), freeDeviceIDs(state), "trial %d, agent %s", trial, id)
		}
	}
	// The key moved some soft tasks to another agent than a plain task's.
	require.Greater(t, moved, 0)
	t.Logf("%d requests planned; %d soft ones on one agent, %d of them on another agent than a plain "+
		"task's", planned, softPlanned, moved)
}
