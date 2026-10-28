//go:build integration
// +build integration

package agentrm

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// awaitStartMessage returns what the master sends the agent to start the allocation's container.
func (ta *restoreTestAgent) awaitStartMessage(id model.AllocationID) *aproto.StartContainer {
	ta.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case msg := <-ta.socket.Inbox:
			if msg.StartContainer == nil {
				continue
			}
			ta.agent.mu.Lock()
			allocationID := ta.agent.agentState.containerAllocation[msg.StartContainer.Container.ID]
			ta.agent.mu.Unlock()
			if allocationID == id {
				return msg.StartContainer
			}
		case <-timeout:
			require.FailNow(ta.t, "no container started", "allocation %s", id)
		}
	}
}

// containerEnvVar returns the value of a variable of the container's environment.
func containerEnvVar(start *aproto.StartContainer, name string) (string, bool) {
	for _, kv := range start.Spec.RunSpec.ContainerConfig.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// A master restarts while a task holds the RTX 4090 that a task with gpu_groups needs besides the
// agent's RTX 6000 Ada. The restored task keeps its groups, waits, and starts on both GPUs once
// the other task ends; its container gets the reserved assignment in DET_GPU_GROUPS.
func TestGPUGroupsQueuedAcrossMasterRestart(t *testing.T) {
	database, dropDB := db.MustResolveNewPostgresDatabase(t)
	t.Cleanup(dropDB)
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	owner := db.RequireMockUser(t, database)
	poolName := "groups-" + uuid.NewString()[:8]
	agentID := aproto.ID(poolName + "-agent")
	devices := []device.Device{
		{ID: 0, Brand: rtx4090, UUID: "GPU-" + string(agentID) + "-0", Type: device.CUDA},
		{ID: 1, Brand: ada6000, UUID: "GPU-" + string(agentID) + "-1", Type: device.CUDA},
	}
	agentStarted := func(reattached ...aproto.ContainerReattachAck) *aproto.AgentStarted {
		return &aproto.AgentStarted{
			Version: "test", ResourcePoolName: poolName, Devices: devices,
			ContainersReattached: reattached,
		}
	}

	first := newRestoreTestMaster(t, poolName, false)
	firstAgent := connectRestoreTestAgent(t, first.addAgent(t, agentID, nil), agentStarted())
	running := newRestoreTestRequest(t, poolName)
	queued := newRestoreTestRequest(t, poolName)
	queued.SlotsNeeded = 2
	queued.FittingRequirements.GPUGroups = simAndLearner
	restoreTestAllocation(t, first, running, &owner)
	runningContainer := firstAgent.awaitStart(running.AllocationID)
	require.Equal(t, devices[:1], runningContainer.Devices)
	firstAgent.report(runningContainer, cproto.Running)
	requireAllocationState(t, running.AllocationID, model.AllocationStateRunning)
	restoreTestAllocation(t, first, queued, &owner)
	time.Sleep(4 * actionCoolDown)
	requireAllocationState(t, queued.AllocationID, model.AllocationStatePending)

	detach(t, running.AllocationID, queued.AllocationID)
	states, err := retrieveAgentStates()
	require.NoError(t, err)
	state, ok := states[agentID]
	require.True(t, ok)
	second := newRestoreTestMaster(t, poolName, false)
	restoredAgent := second.addAgent(t, agentID, &state)
	running.Restore = true
	queued.Restore = true
	restoreTestAllocation(t, second, running, &owner)
	restoreTestAllocation(t, second, queued, &owner)
	t.Cleanup(func() {
		ids := slices.DeleteFunc(
			[]model.AllocationID{running.AllocationID, queued.AllocationID},
			func(id model.AllocationID) bool {
				return !slices.Contains(task.DefaultService.GetAllAllocationIDs(), id)
			},
		)
		detach(t, ids...)
	})

	// Passes run on the restored agent before it reconnects: its devices' models say the task can
	// start later, so it waits instead of failing.
	time.Sleep(4 * actionCoolDown)
	requireAllocationState(t, queued.AllocationID, model.AllocationStatePending)
	jobs, err := second.rm.GetJobQ("")
	require.NoError(t, err)
	require.Equal(t, sproto.SchedulingStateQueued, jobs[queued.JobID].State)

	secondAgent := connectRestoreTestAgent(t, restoredAgent, agentStarted(aproto.ContainerReattachAck{
		Container: cproto.Container{
			ID: runningContainer.ID, State: cproto.Running, Devices: runningContainer.Devices,
		},
	}))
	secondAgent.report(runningContainer, cproto.Terminated)
	start := secondAgent.awaitStartMessage(queued.AllocationID)
	require.Equal(t, devices, start.Container.Devices)
	groups, ok := containerEnvVar(start, gpuGroupsEnvVar)
	require.True(t, ok)
	require.Equal(t, `{"learner":["`+devices[1].UUID+`"],"simulation":["`+devices[0].UUID+`"]}`, groups)
	_, ok = containerEnvVar(start, gpuGroupsEnvVar+"_")
	require.False(t, ok)
	requireAllocationState(t, queued.AllocationID, model.AllocationStateAssigned)
}

func TestValidateGPUGroupsAtSubmit(t *testing.T) {
	validate := func(h *gpuHarness, groups expconf.GPUGroupRequests, pref expconf.GPUTopologyPreference) error {
		_, err := h.rm.ValidateResources(sproto.ValidateResourcesRequest{
			ResourcePool: h.pool, Slots: groups.Slots(), GPUTopology: pref, GPUGroups: groups,
		})
		return err
	}
	// node02's layout: RTX 4090s on NUMA node 0 (GPUs 0-3) and RTX 6000 Ada on node 1 (4-7).
	brands := []string{rtx4090, rtx4090, rtx4090, rtx4090, ada6000, ada6000, ada6000, ada6000}
	h := newDevicesHarness(t, modelDevices(brands...), node02.build(), priorityScheduler42(best, nil))
	cause := func(groups expconf.GPUGroupRequests) string {
		return "no agent in pool " + h.pool + " has GPUs for gpu_groups " + groups.String()
	}
	require.NoError(t, validate(h, simAndLearner, ""))
	require.NoError(t, validate(h, simAndLearner, expconf.GPUTopologySoft))
	four := expconf.GPUGroupRequests{group("sim", 4, rtx4090)}
	require.NoError(t, validate(h, four, expconf.GPUTopologyStrong))
	require.EqualError(t, validate(h, simAndLearner, expconf.GPUTopologyStrong), cause(simAndLearner))
	h100 := expconf.GPUGroupRequests{group("sim", 1, "NVIDIA H100 80GB HBM3")}
	require.EqualError(t, validate(h, h100, ""), cause(h100))
	five := expconf.GPUGroupRequests{group("sim", 5, rtx4090)}
	require.EqualError(t, validate(h, five, ""), cause(five))
	nine := expconf.GPUGroupRequests{group("sim", 5, rtx4090, ada6000), group("learner", 4, ada6000)}
	require.EqualError(t, validate(h, nine, ""), cause(nine), "the single-node check gives the cause")

	// Disabled slots count.
	for _, id := range []device.ID{4, 5, 6, 7} {
		_, err := h.agent.PatchSlotState(patchSlotState{id: id, enabled: ptrs.Ptr(false)})
		require.NoError(t, err)
	}
	require.NoError(t, validate(h, simAndLearner, ""))

	// Before the agent reports its topology, the models decide; with "strong", a task that the
	// models admit is accepted.
	h.agent.mu.Lock()
	h.agent.agentState.setGPUTopology(nil)
	h.agent.mu.Unlock()
	require.NoError(t, validate(h, simAndLearner, expconf.GPUTopologyStrong))
	require.EqualError(t, validate(h, h100, expconf.GPUTopologyStrong), cause(h100))
	require.EqualError(t, validate(h, h100, ""), cause(h100))

	// A pool without agents refuses it, as the single-node check does.
	require.NoError(t, h.rm.agentService.agents.Delete(h.agent.id))
	require.EqualError(t, validate(h, simAndLearner, ""), cause(simAndLearner))
}
