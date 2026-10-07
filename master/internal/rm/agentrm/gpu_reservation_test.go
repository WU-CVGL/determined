package agentrm

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
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
	called := 0
	defer replaceSelection(func(gpuSelectionInput, int, deviceSelection) gpuChoice {
		called++
		return gpuChoice{}
	})()
	state := topologyAgentState(t, node02)
	cid := cproto.NewID()
	res, err := state.allocateFreeDevices(0, cid, deviceSelection{packNUMA: true, preferTopology: true})
	require.NoError(t, err)
	require.Empty(t, res.devices)
	require.Equal(t, &cproto.Container{ID: cid}, state.containerState[cid])
	require.Equal(t, 1, state.numUsedZeroSlots())
	require.Zero(t, called)
}

func replaceSelection(f func(gpuSelectionInput, int, deviceSelection) gpuChoice) func() {
	old := selectFreeDevicesFunc
	selectFreeDevicesFunc = f
	return func() { selectFreeDevicesFunc = old }
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
			defer replaceSelection(func(in gpuSelectionInput, _ int, _ deviceSelection) gpuChoice {
				return gpuChoice{devices: devices(in), rule: "injected"}
			})()
			state := topologyAgentState(t, node02)
			busy := cproto.NewID()
			state.Devices[gpuDevice(0)] = &busy
			res, err := state.allocateFreeDevices(2, cproto.NewID(), packing)
			require.NoError(t, err)
			require.NotEmpty(t, res.failure)
			require.Empty(t, res.choice.rule)
			require.Len(t, res.devices, 2)
			require.Equal(t, 3, state.numUsedSlots())
		})
	}

	t.Run("panic", func(t *testing.T) {
		defer replaceSelection(func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			panic("injected")
		})()
		state := topologyAgentState(t, node02)
		res, err := state.allocateFreeDevices(3, cproto.NewID(), packing)
		require.NoError(t, err)
		require.Contains(t, res.failure, "panic: injected")
		require.Len(t, res.devices, 3)
	})
}

func TestReservationInMapOrderNeverSelects(t *testing.T) {
	// Under worst, or with numa_packing off, a plain task takes map order: no selection runs.
	called := 0
	defer replaceSelection(func(in gpuSelectionInput, n int, sel deviceSelection) gpuChoice {
		called++
		return selectFreeDevices(in, n, sel)
	})()
	state := topologyAgentState(t, node02)
	for i := 0; i < 4; i++ {
		res, err := state.allocateFreeDevices(2, cproto.NewID(), gpuPolicy{}.selection(
			&sproto.AllocateRequest{SlotsNeeded: 2}, []*fittingState{{Agent: state, Slots: 2}}))
		require.NoError(t, err)
		require.Len(t, res.devices, 2)
		require.Empty(t, res.choice.rule)
	}
	require.Zero(t, called)
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
			fits := findFits(req, copies, BestFit, false)
			require.Len(t, fits, 1)
			p.addTaskToAgents(req, fits)
			planned := idsMinus(before, freeDeviceIDs(copies["a"]))

			liveFits := findFits(req, live, BestFit, false)
			res, err := live["a"].allocateFreeDevices(n, cproto.NewID(), policy.selection(req, liveFits))
			require.NoError(t, err)
			require.Equal(t, planned, deviceIDs(res.devices), "request %d", i)
		}
	}
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
	// Packing changes which devices are free, never how many: the fits of two states that differ
	// only in which devices are free are the same.
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
		x := findFits(req, agentsWith(0, 1), BestFit, false)
		y := findFits(req, agentsWith(3, 6), BestFit, false)
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
