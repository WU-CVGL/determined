package agentrm

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// heldDevices are the IDs of the agent's devices that a container holds.
func heldDevices(state *agentState) []device.ID {
	var ids []device.ID
	for d, containerID := range state.Devices {
		if containerID != nil {
			ids = append(ids, d.ID)
		}
	}
	return ids
}

// TestGetJobQPlacement: both schedulers list the devices each job holds, by agent.
func TestGetJobQPlacement(t *testing.T) {
	priority := 42
	for name, scheduler := range map[string]*config.SchedulerConfig{
		"priority": {
			Priority:      &config.PrioritySchedulerConfig{DefaultPriority: &priority},
			FittingPolicy: best,
		},
		"fair share": {FairShare: &config.FairShareSchedulerConfig{}, FittingPolicy: best},
	} {
		t.Run(name, func(t *testing.T) {
			node01 := &MockAgent{ID: "node01", SlotType: "cuda", Slots: 8}
			node02 := &MockAgent{ID: "node02", SlotType: "cuda", Slots: 4, MaxZeroSlotContainers: 1}
			groups := []*MockGroup{
				{ID: "exp", Priority: &priority, Weight: 1},
				{ID: "shell", Priority: &priority, Weight: 1},
				{ID: "tb", Priority: &priority, Weight: 1},
			}
			tasks := []*MockTask{
				// Two trials hold two GPUs each; a third one is queued.
				{
					ID: "exp.1", JobID: "exp", Group: groups[0], SlotsNeeded: 2,
					AllocatedAgent: node01, ContainerStarted: true,
				},
				{
					ID: "exp.2", JobID: "exp", Group: groups[0], SlotsNeeded: 2,
					AllocatedAgent: node01, ContainerStarted: true,
				},
				{ID: "exp.3", JobID: "exp", Group: groups[0], SlotsNeeded: 2},
				{
					ID: "shell.1", JobID: "shell", Group: groups[1], SlotsNeeded: 1,
					AllocatedAgent: node02, ContainerStarted: true,
				},
				{
					ID: "tb.1", JobID: "tb", Group: groups[2], SlotsNeeded: 0,
					AllocatedAgent: node02, ContainerStarted: true,
				},
			}
			conf := &config.ResourcePoolConfig{PoolName: "pool", Scheduler: scheduler}
			rp := setupResourcePool(t, nil, conf, tasks, groups, []*MockAgent{node01, node02})
			// No scheduling pass: it would replace the agents the tasks were placed on.
			rp.stop()
			agents := rp.agentStatesCache

			jobQ := rp.GetJobQ()
			require.Len(t, jobQ, 3)

			exp := jobQ["exp"]
			require.Equal(t, 6, exp.RequestedSlots)
			require.Len(t, exp.Placement, 1)
			require.Len(t, exp.Placement["node01"], 4)
			require.ElementsMatch(t, heldDevices(agents["node01"]),
				exp.Placement["node01"])

			shell := jobQ["shell"]
			require.Equal(t, map[aproto.ID][]device.ID{
				"node02": heldDevices(agents["node02"]),
			}, shell.Placement)
			require.Len(t, shell.Placement["node02"], 1)

			require.Nil(t, jobQ["tb"].Placement)
		})
	}
}
