package agentrm

import (
	"fmt"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/syncx/queue"
)

// newSlotsAgentState returns an agent state with n enabled, free slots, set up as agentStarted
// does but without the database.
func newSlotsAgentState(t *testing.T, n int) (*agentState, []device.Device) {
	t.Helper()
	state := newAgentState(aproto.ID("drain-"+uuid.NewString()), 100)
	state.handler = &agent{}
	devices := make([]device.Device, 0, n)
	for i := 0; i < n; i++ {
		d := device.Device{
			ID: device.ID(i), Brand: "nvda", UUID: fmt.Sprintf("GPU-%d", i), Type: device.CUDA,
		}
		devices = append(devices, d)
		state.slotStates[d.ID] = &slot{
			device:  d,
			enabled: slotEnabled{agentEnabled: true, userEnabled: true},
		}
		state.updateSlotDeviceView(d.ID)
	}
	return state, devices
}

func drainSlot(t *testing.T, state *agentState, id device.ID) {
	t.Helper()
	summary, err := state.patchSlotState(patchSlotState{
		id: id, enabled: ptrs.Ptr(false), drain: ptrs.Ptr(true),
	})
	require.NoError(t, err)
	require.False(t, summary.Enabled)
	require.True(t, summary.Draining)
}

// freeDeviceIDs is the allocatable set: the free entries of Devices.
func freeDeviceIDs(state *agentState) []device.ID {
	var ids []device.ID
	for d, cid := range state.Devices {
		if cid == nil {
			ids = append(ids, d.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// requireOnlyAllocatable checks that the scheduler's count and the reservation both see exactly
// the slots in want, and that a reservation of one more slot fails.
func requireOnlyAllocatable(t *testing.T, state *agentState, want ...device.ID) {
	t.Helper()
	require.Equal(t, want, freeDeviceIDs(state))
	require.Equal(t, len(want), state.numEmptySlots())

	tooMany := cproto.NewID()
	_, err := state.allocateFreeDevices(len(want)+1, tooMany)
	require.Error(t, err, "reserved more slots than are allocatable")
	state.deallocateContainer(tooMany)

	cid := cproto.NewID()
	got, err := state.allocateFreeDevices(len(want), cid)
	require.NoError(t, err)
	gotIDs := make([]device.ID, 0, len(got))
	for _, d := range got {
		gotIDs = append(gotIDs, d.ID)
	}
	sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
	if len(want) == 0 {
		require.Empty(t, gotIDs)
	} else {
		require.Equal(t, want, gotIDs)
	}
	state.deallocateContainer(cid)
}

func TestDrainIdleSlotIsNotAllocatable(t *testing.T) {
	state, _ := newSlotsAgentState(t, 2)

	drainSlot(t, state, 0)

	requireOnlyAllocatable(t, state, 1)
	require.Equal(t, 1, state.numSlots())
	require.True(t, state.getSlotSummary(0).Draining)
}

func TestDrainRunningSlotIsNotAllocatableAfterItsTaskExits(t *testing.T) {
	state, devices := newSlotsAgentState(t, 2)

	// A task runs on both slots: the pool reserves them, and the agent starts the container.
	cid := cproto.NewID()
	_, err := state.allocateFreeDevices(2, cid)
	require.NoError(t, err)
	for _, d := range devices {
		state.slotStates[d.ID].containerID = &cid
	}

	// Draining slot 0 lets the task run on.
	drainSlot(t, state, 0)
	require.Equal(t, &cid, state.Devices[devices[0]])
	require.Equal(t, 2, state.numUsedSlots())
	requireOnlyAllocatable(t, state)

	// The task exits: the agent reports it terminated, then the pool releases its resources.
	for _, d := range devices {
		state.slotStates[d.ID].containerID = nil
	}
	state.deallocateContainer(cid)

	requireOnlyAllocatable(t, state, 1)
	require.Equal(t, 1, state.numSlots())
	require.True(t, state.getSlotSummary(0).Draining)
}

func TestDrainReservedSlotIsNotAllocatableAfterTheReservationIsCanceled(t *testing.T) {
	state, devices := newSlotsAgentState(t, 2)

	// The pool reserves both slots; no container has started yet.
	cid := cproto.NewID()
	_, err := state.allocateFreeDevices(2, cid)
	require.NoError(t, err)

	// Draining slot 0 keeps the reservation.
	drainSlot(t, state, 0)
	require.Equal(t, &cid, state.Devices[devices[0]])
	requireOnlyAllocatable(t, state)

	// The allocation goes away before it starts.
	state.deallocateContainer(cid)

	requireOnlyAllocatable(t, state, 1)
}

func TestEnableDrainedSlotMakesItAllocatable(t *testing.T) {
	state, _ := newSlotsAgentState(t, 2)
	drainSlot(t, state, 0)

	summary, err := state.patchSlotState(patchSlotState{id: 0, enabled: ptrs.Ptr(true)})
	require.NoError(t, err)
	require.True(t, summary.Enabled)
	require.False(t, summary.Draining, "an enabled slot still reports draining")

	requireOnlyAllocatable(t, state, 0, 1)
	require.Equal(t, 2, state.numSlots())
}

// runOnOneSlot starts a task on one slot of a two-slot agent: the pool reserves a slot, and the
// agent starts the container. It returns the container, the slot in use and the other slot.
func runOnOneSlot(t *testing.T, state *agentState) (cproto.ID, device.ID, device.ID) {
	t.Helper()
	cid := cproto.NewID()
	got, err := state.allocateFreeDevices(1, cid)
	require.NoError(t, err)
	require.Len(t, got, 1)
	used := got[0].ID
	state.slotStates[used].containerID = &cid
	state.containerAllocation[cid] = model.AllocationID("drain-" + string(cid))
	return cid, used, 1 - used
}

// When the pool releases a task's container before the agent reports it terminated, and the slot
// is enabled in between, the slot must come back free: the pool will not release that container
// again.
func TestEnableSlotAfterItsContainerIsReleasedBeforeItTerminates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drain bool
	}{
		{name: "disabled slot", drain: false},
		{name: "drained slot", drain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, _ := newSlotsAgentState(t, 2)
			cid, used, _ := runOnOneSlot(t, state)

			_, err := state.patchSlotState(patchSlotState{
				id: used, enabled: ptrs.Ptr(false), drain: ptrs.Ptr(tc.drain),
			})
			require.NoError(t, err)

			// The pool releases the container; the agent has not reported it terminated yet.
			state.deallocateContainer(cid)
			require.Equal(t, &cid, state.slotStates[used].containerID)

			_, err = state.patchSlotState(patchSlotState{id: used, enabled: ptrs.Ptr(true)})
			require.NoError(t, err)

			// The agent reports the container terminated.
			state.slotStates[used].containerID = nil

			requireOnlyAllocatable(t, state, 0, 1)
			require.Equal(t, 2, state.numSlots())
		})
	}
}

// Enabling a disabled slot while its task still runs keeps the slot in use, and the pool's
// release of the task's container frees it.
func TestEnableDisabledSlotWhileItsTaskRuns(t *testing.T) {
	state, devices := newSlotsAgentState(t, 2)
	cid, used, other := runOnOneSlot(t, state)

	_, err := state.patchSlotState(patchSlotState{id: used, enabled: ptrs.Ptr(false)})
	require.NoError(t, err)
	requireOnlyAllocatable(t, state, other)

	_, err = state.patchSlotState(patchSlotState{id: used, enabled: ptrs.Ptr(true)})
	require.NoError(t, err)
	require.Equal(t, &cid, state.Devices[devices[used]])
	requireOnlyAllocatable(t, state, other)

	state.deallocateContainer(cid)
	requireOnlyAllocatable(t, state, 0, 1)
}

// The priority scheduler simulates preemption on copies of the agent states. Preempting a task on
// a draining slot frees nothing that a pending task can use, so on a one-slot agent, where that
// task is the only candidate, nothing is preempted. The simulation must also leave the real agent
// state alone. With more candidates, trySchedulingTaskViaPreemption still preempts every candidate
// it removed before the one that made the request fit, so a task on a draining slot can be
// preempted together with that one (unchanged, not covered here).
func TestPrioritySchedulerDoesNotPreemptForADrainingSlot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		drain       bool
		wantRelease bool
	}{
		{name: "enabled slot", drain: false, wantRelease: true},
		{name: "draining slot", drain: true, wantRelease: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, devices := newSlotsAgentState(t, 1)
			lowPriority, highPriority := 50, 40
			groups := map[model.JobID]*tasklist.Group{
				"low":  {JobID: "low", Priority: &lowPriority},
				"high": {JobID: "high", Priority: &highPriority},
			}
			taskList := tasklist.New()

			low := &sproto.AllocateRequest{
				AllocationID: "low", JobID: "low", SlotsNeeded: 1, Name: "low",
				Preemption: sproto.PreemptionConfig{Preemptible: true},
			}
			taskList.AddTask(low)
			cid := cproto.NewID()
			got, err := state.allocateFreeDevices(1, cid)
			require.NoError(t, err)
			state.slotStates[devices[0].ID].containerID = &cid
			taskList.AddAllocation(low.AllocationID, &sproto.ResourcesAllocated{
				ID: low.AllocationID,
				Resources: map[sproto.ResourcesID]sproto.Resources{
					sproto.ResourcesID(cid): &containerResources{
						req: low, agent: state, containerID: cid, devices: got,
					},
				},
			})
			if tc.drain {
				drainSlot(t, state, devices[0].ID)
			}

			high := &sproto.AllocateRequest{
				AllocationID: "high", JobID: "high", SlotsNeeded: 1, Name: "high",
			}
			taskList.AddTask(high)

			p := &priorityScheduler{preemptionEnabled: true}
			toAllocate, toRelease := p.prioritySchedule(
				taskList, groups, make(map[model.JobID]decimal.Decimal),
				map[aproto.ID]*agentState{state.id: state}, BestFit,
			)
			require.Empty(t, toAllocate)
			if tc.wantRelease {
				require.Equal(t, []model.AllocationID{"low"}, toRelease)
			} else {
				require.Empty(t, toRelease)
			}

			// The simulation did not touch the real state.
			require.Equal(t, &cid, state.Devices[devices[0]])
			require.True(t, state.slotStates[devices[0].ID].enabled.deviceAdded)
		})
	}
}

func TestPatchSlotStateReschedulesThePool(t *testing.T) {
	state, _ := newSlotsAgentState(t, 2)
	updates := queue.New[agentUpdatedEvent]()
	a := &agent{
		syslog:           logrus.WithField("component", "agent"),
		id:               state.id,
		agentUpdates:     updates,
		resourcePoolName: "pool",
		started:          true,
		agentState:       state,
	}
	state.handler = a

	// GetSlot reads through an empty patch; a read does not reschedule.
	_, err := a.PatchSlotState(patchSlotState{id: 0})
	require.NoError(t, err)
	require.Zero(t, updates.Len())

	for _, tc := range []struct {
		name  string
		patch patchSlotState
	}{
		{name: "disable", patch: patchSlotState{id: 0, enabled: ptrs.Ptr(false)}},
		{name: "enable", patch: patchSlotState{id: 0, enabled: ptrs.Ptr(true)}},
		{name: "drain", patch: patchSlotState{
			id: 1, enabled: ptrs.Ptr(false), drain: ptrs.Ptr(true),
		}},
		{name: "enable drained", patch: patchSlotState{id: 1, enabled: ptrs.Ptr(true)}},
	} {
		_, err := a.PatchSlotState(tc.patch)
		require.NoError(t, err, tc.name)
		require.Equal(t, 1, updates.Len(), "%s: the pool was not told to reschedule", tc.name)
		require.Equal(t, agentUpdatedEvent{resourcePool: "pool"}, updates.Get())
	}

	// An unknown slot changes nothing.
	_, err = a.PatchSlotState(patchSlotState{id: 7, enabled: ptrs.Ptr(false)})
	require.Error(t, err)
	require.Zero(t, updates.Len())
}
