package agentrm

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

var packing = deviceSelection{packNUMA: true}

// topologyAgentState returns an agent with the slots and the topology of a fixture.
func topologyAgentState(t *testing.T, f topologyFixture) *agentState {
	t.Helper()
	state, _ := newSlotsAgentState(t, 0)
	for _, id := range f.ids {
		d := gpuDevice(id)
		state.slotStates[d.ID] = &slot{device: d, enabled: slotEnabled{agentEnabled: true, userEnabled: true}}
		state.updateSlotDeviceView(d.ID)
	}
	state.setGPUTopology(f.build())
	return state
}

func reserve(t *testing.T, state *agentState, n int, sel deviceSelection) []int {
	t.Helper()
	res, err := state.allocateFreeDevices(n, cproto.NewID(), sel)
	require.NoError(t, err)
	require.Empty(t, res.failure)
	return deviceIDs(res.devices)
}

// stateSnapshot is what a reservation may change.
type stateSnapshot struct {
	devices    map[device.Device]*cproto.ID
	containers map[cproto.ID]*cproto.Container
}

func snapshotOf(state *agentState) stateSnapshot {
	s := stateSnapshot{devices: map[device.Device]*cproto.ID{}, containers: map[cproto.ID]*cproto.Container{}}
	for d, cid := range state.Devices {
		s.devices[d] = cid
	}
	for cid, c := range state.containerState {
		s.containers[cid] = c
	}
	return s
}

func TestReservationPacksAndIsDeterministic(t *testing.T) {
	// Eight 1-slot tasks get slots 0..7 in order; map order does so with probability 8!/8^8.
	state := topologyAgentState(t, node02)
	for want := 0; want < 8; want++ {
		require.Equal(t, []int{want}, reserve(t, state, 1, packing))
	}
	state = topologyAgentState(t, node01)
	var order []int
	for range node01IDs {
		order = append(order, reserve(t, state, 1, packing)...)
	}
	require.Equal(t, []int{5, 6, 7, 0, 1, 2, 3}, order)

	// The same for every Devices map, built anew each time, and across repeated calls.
	for i := 0; i < 50; i++ {
		state := topologyAgentState(t, node02)
		reserve(t, state, 1, packing)
		reserve(t, state, 2, packing)
		require.Equal(t, []int{4, 5, 6, 7}, reserve(t, state, 4, packing))
	}
}

func TestReservationWithoutSlotStatesCountsDevicesAsAllocatable(t *testing.T) {
	// The scheduler test helpers fill Devices only.
	state := newAgentState("no-slots", 0)
	for _, id := range node02IDs {
		state.Devices[gpuDevice(id)] = nil
	}
	state.setGPUTopology(node02.build())
	require.Equal(t, []int{0, 1}, reserve(t, state, 2, packing))
}

func TestReservationShortCountChangesNothing(t *testing.T) {
	for name, sel := range map[string]deviceSelection{
		"map order": {}, "packing": packing, "topology": {preferTopology: true, packNUMA: true},
	} {
		state := topologyAgentState(t, node02)
		reserve(t, state, 6, sel)
		before := snapshotOf(state)
		_, err := state.allocateFreeDevices(3, cproto.NewID(), sel)
		require.Error(t, err, name)
		require.Equal(t, before, snapshotOf(state), name)
	}
}

func TestReservationZeroSlotsReadsNoTopology(t *testing.T) {
	state := topologyAgentState(t, node02)
	cid := cproto.NewID()
	res, err := state.allocateFreeDevices(0, cid, deviceSelection{packNUMA: true, preferTopology: true})
	require.NoError(t, err)
	// No selection ran: one gives a set or a reason.
	require.Equal(t, deviceReservation{}, res)
	require.Equal(t, &cproto.Container{ID: cid}, state.containerState[cid])
	require.Equal(t, 1, state.numUsedZeroSlots())
}

func TestReservationFallsBackToMapOrder(t *testing.T) {
	invalid := map[string]func(in gpuSelectionInput) []device.Device{
		"short":     func(in gpuSelectionInput) []device.Device { return in.free[:1] },
		"duplicate": func(in gpuSelectionInput) []device.Device { return []device.Device{in.free[0], in.free[0]} },
		"absent":    func(in gpuSelectionInput) []device.Device { return []device.Device{in.free[0], gpuDevice(42)} },
		"busy": func(in gpuSelectionInput) []device.Device {
			return []device.Device{in.free[0], gpuDevice(0)}
		},
	}
	for name, devices := range invalid {
		t.Run(name, func(t *testing.T) {
			state := topologyAgentState(t, node02)
			busy := cproto.NewID()
			state.Devices[gpuDevice(0)] = &busy
			res, err := state.chooseFreeDevices(2, packing,
				func(in gpuSelectionInput, _ int, _ deviceSelection) gpuChoice {
					return gpuChoice{devices: devices(in), rule: "injected"}
				})
			require.NoError(t, err)
			require.NotEmpty(t, res.failure)
			require.Empty(t, res.choice.rule)
			require.NoError(t, state.checkFreeDevices(res.devices, 2))
		})
	}

	t.Run("no set and no reason", func(t *testing.T) {
		state := topologyAgentState(t, node02)
		res, err := state.chooseFreeDevices(2, packing, func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			return gpuChoice{}
		})
		require.NoError(t, err)
		require.Equal(t, "no devices and no reason", res.failure)
		require.NoError(t, state.checkFreeDevices(res.devices, 2))
	})

	t.Run("panic", func(t *testing.T) {
		state := topologyAgentState(t, node02)
		res, err := state.chooseFreeDevices(3, packing, func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			panic("injected")
		})
		require.NoError(t, err)
		require.Contains(t, res.failure, "panic: injected")
		require.NoError(t, state.checkFreeDevices(res.devices, 3))
	})
}

func TestReservationUnrankedTakesMapOrderWithItsReason(t *testing.T) {
	// "soft" without packing on an agent without a topology, or above the set cap: map order, with
	// the reason and no failure.
	state := topologyAgentState(t, node02)
	state.gpuTopology = nil
	soft := deviceSelection{preferTopology: true}
	res, err := state.allocateFreeDevices(3, cproto.NewID(), soft)
	require.NoError(t, err)
	require.Empty(t, res.failure)
	require.Nil(t, res.choice.devices)
	require.Equal(t, "topology unknown: "+reasonNotReportedSinceMasterStart, res.choice.mapOrder)
	require.Equal(t, res.choice.mapOrder, res.choice.unranked)
	require.Len(t, res.devices, 3)
	require.Equal(t, 3, state.numUsedSlots())

	ids := intRange(0, 17)
	state = topologyAgentState(t, topologyFixture{ids: ids, numa: twoSockets(ids), p2p: allP2P(p2pOK)})
	res, err = state.allocateFreeDevices(8, cproto.NewID(), soft)
	require.NoError(t, err)
	require.Empty(t, res.failure)
	require.Equal(t, "more than 20000 sets of free GPUs", res.choice.mapOrder)
	require.Len(t, res.devices, 8)

	// 1 slot: "soft" does not apply.
	res, err = state.allocateFreeDevices(1, cproto.NewID(), soft)
	require.NoError(t, err)
	require.Equal(t, "fewer than 2 slots", res.choice.mapOrder)
	require.Empty(t, res.choice.unranked)
}

func TestChooseFreeDevicesChangesNothing(t *testing.T) {
	// The reservation's one change of state comes after the choice: choosing, whatever the
	// selection does, changes nothing and gives a full set of free devices.
	selections := map[string]func(gpuSelectionInput, int, deviceSelection) gpuChoice{
		"ranked":  selectFreeDevices,
		"invalid": func(in gpuSelectionInput, _ int, _ deviceSelection) gpuChoice { return gpuChoice{devices: in.free[:1]} },
		"reason":  func(gpuSelectionInput, int, deviceSelection) gpuChoice { return gpuChoice{mapOrder: "injected"} },
		"panic":   func(gpuSelectionInput, int, deviceSelection) gpuChoice { panic("injected") },
	}
	for name, selector := range selections {
		for _, sel := range []deviceSelection{{}, packing, {preferTopology: true}} {
			state := topologyAgentState(t, node02)
			busy := cproto.NewID()
			state.Devices[gpuDevice(2)] = &busy
			before := snapshotOf(state)
			res, err := state.chooseFreeDevices(4, sel, selector)
			require.NoError(t, err, name)
			require.Equal(t, before, snapshotOf(state), name)
			require.NoError(t, state.checkFreeDevices(res.devices, 4), name)
		}
	}
}

func TestReservationInMapOrderNeverSelects(t *testing.T) {
	// Under worst, or with numa_packing off, a plain task takes map order: no selection runs (a
	// selection here panics, which would be a failure).
	panics := func(gpuSelectionInput, int, deviceSelection) gpuChoice { panic("selected") }
	state := topologyAgentState(t, node02)
	for i := 0; i < 4; i++ {
		sel := gpuPolicy{}.selection(
			&sproto.AllocateRequest{SlotsNeeded: 2}, []*fittingState{{Agent: state, Slots: 2}})
		res, err := state.chooseFreeDevices(2, sel, panics)
		require.NoError(t, err)
		require.Equal(t, deviceReservation{devices: res.devices}, res)
		require.Len(t, res.devices, 2)
		_, err = state.allocateFreeDevices(2, cproto.NewID(), sel)
		require.NoError(t, err)
	}
	require.Zero(t, state.numEmptySlots())
}

func TestReservationOverRandomStates(t *testing.T) {
	// Over random agent states with busy, draining and disabled slots, a packed reservation and one
	// in map order succeed or fail together, and a packed one takes allocatable slots, sorted.
	rng := rand.New(rand.NewSource(7)) //nolint:gosec
	for trial := 0; trial < 300; trial++ {
		build := func() *agentState {
			r := rand.New(rand.NewSource(int64(trial))) //nolint:gosec
			state := topologyAgentState(t, node02)
			for _, id := range node02IDs {
				d := gpuDevice(id)
				switch r.Intn(5) {
				case 0: // busy
					cid := cproto.NewID()
					state.Devices[d] = &cid
				case 1: // busy and draining
					cid := cproto.NewID()
					state.Devices[d] = &cid
					drainSlot(t, state, d.ID)
				case 2: // disabled
					_, err := state.patchSlotState(patchSlotState{id: d.ID, enabled: ptrs.Ptr(false)})
					require.NoError(t, err)
				}
			}
			return state
		}
		n := 1 + rng.Intn(6)
		plain, packed := build(), build()
		_, errPlain := plain.allocateFreeDevices(n, cproto.NewID(), deviceSelection{})
		res, errPacked := packed.allocateFreeDevices(n, cproto.NewID(), packing)
		require.Equal(t, errPlain == nil, errPacked == nil, "trial %d, n=%d", trial, n)
		if errPacked != nil {
			continue
		}
		require.Empty(t, res.failure)
		ids := deviceIDs(res.devices)
		require.True(t, sort.IntsAreSorted(ids))
		for _, d := range res.devices {
			require.True(t, packed.slotStates[d.ID].enabled.allocatable(), "slot %d", d.ID)
		}
	}
}

func TestSimulationChoosesTheLiveDevices(t *testing.T) {
	// The priority scheduler's simulation places requests on copies with the pass's GPU policy;
	// the live reservations of the pass, in the same order, choose the same devices.
	for _, policy := range []gpuPolicy{
		{packNUMA: true},
		{packNUMA: true, xids: map[string]bool{gpuDevice(1).UUID: true}},
	} {
		live := map[aproto.ID]*agentState{"a": topologyAgentState(t, node01)}
		live["a"].id = "a"
		copies := deepCopyAgents(live)
		p := priorityScheduler{gpus: policy}
		for i, n := range []int{1, 2, 1, 3} {
			req := &sproto.AllocateRequest{
				AllocationID: model.AllocationID(fmt.Sprintf("task-%d", i)), SlotsNeeded: n,
				FittingRequirements: sproto.FittingRequirements{SingleAgent: true},
			}
			before := freeDeviceIDs(copies["a"])
			fits := findFits(req, copies, BestFit, false, policy.packNUMA)
			require.Len(t, fits, 1)
			p.addTaskToAgents(req, fits)
			planned := idsMinus(before, freeDeviceIDs(copies["a"]))

			liveFits := findFits(req, live, BestFit, false, policy.packNUMA)
			res, err := live["a"].allocateFreeDevices(n, cproto.NewID(), policy.selection(req, liveFits))
			require.NoError(t, err)
			require.Equal(t, planned, deviceIDs(res.devices), "request %d", i)
		}
	}
}

func TestPrioritySchedulePassPlansTheLiveDevices(t *testing.T) {
	// The live reservations of the requests that a pass through the pool's priority scheduler
	// plans, in the order the pass returns them and with the pass's GPU policy (packing, the pass's
	// XIDs, "soft"), choose the devices that the simulation (addTaskToAgents) chooses on copies of
	// the agents with that policy. Preemption is off.
	conf := &config.ResourcePoolConfig{PoolName: "pool", Scheduler: &config.SchedulerConfig{
		Priority:      &config.PrioritySchedulerConfig{DefaultPriority: ptrs.Ptr(42)},
		FittingPolicy: best,
	}}
	var tasks []*MockTask
	var groups []*MockGroup
	for i, n := range []int{1, 2, 1, 3, 4, 1, 2} {
		// Two priorities: the pass places the higher one first.
		group := &MockGroup{ID: fmt.Sprintf("job-%d", i), Priority: ptrs.Ptr(10 + 32*(i%2))}
		groups = append(groups, group)
		tasks = append(tasks, &MockTask{
			ID: model.AllocationID(fmt.Sprintf("task-%d", i)), SlotsNeeded: n, Group: group,
		})
	}
	rp := setupResourcePool(t, nil, conf, tasks, groups, nil)
	soft, ok := rp.taskList.TaskByID("task-1")
	require.True(t, ok)
	soft.FittingRequirements.GPUTopology = expconf.GPUTopologySoft

	live := map[aproto.ID]*agentState{}
	for id, f := range map[aproto.ID]topologyFixture{"a": node01, "b": node02} {
		state := topologyAgentState(t, f)
		state.id = id
		busy := cproto.NewID()
		state.Devices[gpuDevice(6)] = &busy
		live[id] = state
	}
	rp.agentStatesCache = live
	rp.gpuPolicy = gpuPolicy{packNUMA: true, xids: map[string]bool{gpuDevice(1).UUID: true}}

	toAllocate, toRelease := rp.scheduler.Schedule(rp)
	require.Empty(t, toRelease)
	require.NotEmpty(t, toAllocate)

	copies := deepCopyAgents(live)
	simulation := priorityScheduler{gpus: rp.gpuPolicy}
	sawSoft := false
	for _, req := range toAllocate {
		fits := findFits(req, live, rp.fittingMethod, false, rp.gpuPolicy.packNUMA)
		require.Len(t, fits, 1)
		sel := rp.gpuPolicy.selection(req, fits)
		sawSoft = sawSoft || sel.preferTopology

		copyFits := findFits(req, copies, rp.fittingMethod, false, rp.gpuPolicy.packNUMA)
		require.Len(t, copyFits, 1)
		require.Equal(t, fits[0].Agent.id, copyFits[0].Agent.id)
		before := freeDeviceIDs(copyFits[0].Agent)
		require.True(t, simulation.addTaskToAgents(req, copyFits), "request %s", req.AllocationID)
		planned := idsMinus(before, freeDeviceIDs(copyFits[0].Agent))

		res, err := fits[0].Agent.allocateFreeDevices(fits[0].Slots, cproto.NewID(), sel)
		require.NoError(t, err)
		require.Empty(t, res.failure)
		require.Equal(t, planned, deviceIDs(res.devices), "request %s", req.AllocationID)
	}
	require.True(t, sawSoft, "the pass plans the request with \"soft\"")
}

func idsMinus(before, after []device.ID) []int {
	left := map[device.ID]bool{}
	for _, id := range after {
		left[id] = true
	}
	var out []int
	for _, id := range before {
		if !left[id] {
			out = append(out, int(id))
		}
	}
	return out
}

func TestFitsDependOnCountsOnly(t *testing.T) {
	// Packing changes which devices are free, never how many: for a request without
	// prefer_gpu_topology, the fits of two states that differ only in which devices are free are the
	// same, with the pool's gate on too.
	agentsWith := func(busy ...int) map[aproto.ID]*agentState {
		out := map[aproto.ID]*agentState{}
		for _, id := range []aproto.ID{"a", "b"} {
			state := topologyAgentState(t, node02)
			state.id = id
			for _, b := range busy {
				cid := cproto.NewID()
				state.Devices[gpuDevice(b)] = &cid
			}
			out[id] = state
		}
		return out
	}
	for _, n := range []int{1, 2, 4, 6, 12} {
		req := &sproto.AllocateRequest{AllocationID: "r", SlotsNeeded: n}
		x := findFits(req, agentsWith(0, 1), BestFit, false, true)
		y := findFits(req, agentsWith(3, 6), BestFit, false, true)
		require.Equal(t, len(x), len(y), "n=%d", n)
		for i := range x {
			require.Equal(t, x[i].Agent.id, y[i].Agent.id)
			require.Equal(t, x[i].Slots, y[i].Slots)
		}
	}
}

func TestPackingFeedsBackThroughSlotDrain(t *testing.T) {
	// Idle 8-GPU agent, one 1-slot task, then slot 0 drains: under packing the task holds slot 0,
	// so 7 slots stay empty and a 7-slot task fits. In map order the task usually holds another
	// slot, slot 0 leaves at once, and only 6 stay empty.
	state := topologyAgentState(t, node02)
	require.Equal(t, []int{0}, reserve(t, state, 1, packing))
	drainSlot(t, state, 0)
	require.Equal(t, 7, state.numEmptySlots())
}
