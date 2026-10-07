//go:build integration
// +build integration

package agentrm

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/logger"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/syncx/queue"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/master/pkg/ws"
)

// restoreTestMaster is the part of a master that schedules allocations: an agent resource manager
// with one pool.
type restoreTestMaster struct {
	rm           *ResourceManager
	agents       *agents
	agentUpdates *queue.Queue[agentUpdatedEvent]
	pool         config.ResourcePoolConfig
}

func newRestoreTestMaster(t *testing.T, poolName string) *restoreTestMaster {
	t.Helper()
	pool := config.ResourcePoolConfig{
		PoolName:                 poolName,
		MaxAuxContainersPerAgent: 100,
		AgentReconnectWait:       model.Duration(time.Hour),
	}
	rmConfig := &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{
			AgentRM: &config.AgentResourceManagerConfig{
				Scheduler: &config.SchedulerConfig{
					FairShare:     &config.FairShareSchedulerConfig{},
					FittingPolicy: best,
				},
				DefaultComputeResourcePool: poolName,
				DefaultAuxResourcePool:     poolName,
			},
		},
		ResourcePools: []config.ResourcePoolConfig{pool},
	}
	registry, err := newPoolRegistry(rmConfig.ResourcePools)
	require.NoError(t, err)
	agentService, agentUpdates := newAgentService(registry, restoreTestAgentOptions(), false)
	rm, err := newAgentResourceManager(nil, rmConfig, nil, agentService, agentUpdates, registry)
	require.NoError(t, err)
	t.Cleanup(rm.stop)
	return &restoreTestMaster{rm: rm, agents: agentService, agentUpdates: agentUpdates, pool: pool}
}

func restoreTestAgentOptions() *aproto.MasterSetAgentOptions {
	return &aproto.MasterSetAgentOptions{
		LoggingOptions: model.LoggingConfig{DefaultLoggingConfig: &model.DefaultLoggingConfig{}},
	}
}

// addAgent registers an agent with the master; restored is its state from the database, or nil
// for an agent that connects for the first time.
func (m *restoreTestMaster) addAgent(t *testing.T, id aproto.ID, restored *agentState) *agent {
	t.Helper()
	a := newAgent(
		id, m.agentUpdates, m.pool.PoolName, &m.pool, restoreTestAgentOptions(), restored, func() {},
	)
	require.NoError(t, m.agents.agents.Add(id, a))
	return a
}

// restoreTestAgent plays an agent process that is connected to the master over a websocket.
type restoreTestAgent struct {
	t      *testing.T
	agent  *agent
	socket *ws.WebSocket[aproto.AgentMessage, *aproto.MasterMessage]
}

func connectRestoreTestAgent(t *testing.T, a *agent, started *aproto.AgentStarted) *restoreTestAgent {
	t.Helper()
	e := echo.New()
	e.GET("/", func(c echo.Context) error {
		require.NoError(t, a.HandleWebsocketConnection(webSocketRequest{echoCtx: c}))
		return nil
	})
	server := httptest.NewServer(e.Server.Handler)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial(
		fmt.Sprintf("ws://%s", strings.TrimPrefix(server.URL, "http://")), nil,
	)
	require.NoError(t, err)
	socket, err := ws.Wrap[aproto.AgentMessage, *aproto.MasterMessage]("test-agent", conn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = socket.Close() })
	socket.Outbox <- &aproto.MasterMessage{AgentStarted: started}
	return &restoreTestAgent{t: t, agent: a, socket: socket}
}

// awaitStart returns the container that the master starts on the agent for the allocation.
func (ta *restoreTestAgent) awaitStart(id model.AllocationID) cproto.Container {
	ta.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case msg := <-ta.socket.Inbox:
			if msg.StartContainer == nil {
				continue
			}
			c := msg.StartContainer.Container
			ta.agent.mu.Lock()
			allocationID := ta.agent.agentState.containerAllocation[c.ID]
			ta.agent.mu.Unlock()
			if allocationID == id {
				return c
			}
		case <-timeout:
			require.FailNow(ta.t, "no container started", "allocation %s", id)
		}
	}
}

// report sends the container's state to the master.
func (ta *restoreTestAgent) report(c cproto.Container, state cproto.State) {
	c.State = state
	msg := aproto.ContainerStateChanged{Container: c}
	switch state {
	case cproto.Running:
		msg.ContainerStarted = &aproto.ContainerStarted{
			ProxyAddress: "127.0.0.1",
			ContainerInfo: types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{
				ID: string(c.ID), HostConfig: &container.HostConfig{},
			}},
		}
	case cproto.Terminated:
		msg.ContainerStopped = &aproto.ContainerStopped{}
	}
	ta.socket.Outbox <- &aproto.MasterMessage{ContainerStateChanged: &msg}
}

// restoreTestSpec is the task of an allocation.
type restoreTestSpec struct{ owner *model.User }

func (s restoreTestSpec) ToTaskSpec() tasks.TaskSpec {
	return tasks.TaskSpec{
		Owner:                 s.owner,
		AgentUserGroup:        &model.AgentUserGroup{User: "root", Group: "root"},
		TaskContainerDefaults: *model.DefaultTaskContainerDefaults(),
		Environment:           model.DefaultEnvConfig(nil).ToExpconf(),
		ResourcesConfig:       model.DefaultResourcesConfig(nil).ToExpconf(),
		ExtraEnvVars:          map[string]string{},
		WorkDir:               "/",
		TaskType:              model.TaskTypeShell,
	}
}

// restoreTestAllocation starts the allocation of a new one-slot task, as a master does when the
// task is submitted, or with restore, as a master does at startup for an allocation that has
// not ended.
func restoreTestAllocation(
	t *testing.T, m *restoreTestMaster, req sproto.AllocateRequest, owner *model.User,
) {
	t.Helper()
	err := task.DefaultService.StartAllocation(
		logger.Context{"allocation-id": req.AllocationID}, req, db.SingleDB(), m.rm,
		restoreTestSpec{owner: owner}, func(*task.AllocationExited) {},
	)
	require.NoError(t, err)
}

func newRestoreTestRequest(t *testing.T, pool string) sproto.AllocateRequest {
	t.Helper()
	ctx := context.Background()
	taskID := model.TaskID(uuid.NewString())
	jobID := model.JobID(uuid.NewString())
	require.NoError(t, db.AddJob(&model.Job{JobID: jobID, JobType: model.JobTypeShell}))
	require.NoError(t, db.AddTask(ctx, &model.Task{
		TaskID:     taskID,
		TaskType:   model.TaskTypeShell,
		StartTime:  time.Now().UTC(),
		JobID:      &jobID,
		LogVersion: model.CurrentTaskLogVersion,
	}))
	// The job's scheduling group lives while the job is registered, as for a shell.
	require.NoError(t, tasklist.GroupPriorityChangeRegistry.Add(jobID, nil))
	t.Cleanup(func() { _ = tasklist.GroupPriorityChangeRegistry.Delete(jobID) })
	return sproto.AllocateRequest{
		AllocationID:        model.AllocationID(fmt.Sprintf("%s.1", taskID)),
		TaskID:              taskID,
		JobID:               jobID,
		JobSubmissionTime:   time.Now().UTC().Truncate(time.Millisecond),
		IsUserVisible:       true,
		Name:                string(taskID),
		SlotsNeeded:         1,
		ResourcePool:        pool,
		FittingRequirements: sproto.FittingRequirements{SingleAgent: true},
	}
}

func requireAllocationState(t *testing.T, id model.AllocationID, state model.AllocationState) {
	t.Helper()
	require.Eventually(t, func() bool {
		s, err := task.DefaultService.State(id)
		return err == nil && s.State == state
	}, 10*time.Second, 10*time.Millisecond, "allocation %s never reached %s", id, state)
}

// detach lets go of the allocations as a master that stops does.
func detach(t *testing.T, ids ...model.AllocationID) {
	t.Helper()
	for _, id := range ids {
		require.NoError(t, task.DefaultService.Detach(id))
	}
	require.Eventually(t, func() bool {
		live := task.DefaultService.GetAllAllocationIDs()
		return !slices.ContainsFunc(ids, func(id model.AllocationID) bool { return slices.Contains(live, id) })
	}, 10*time.Second, 10*time.Millisecond)
}

// A master restarts while one task runs on the only slot and another waits for it. The new master
// reattaches the running task, keeps the waiting one queued, and starts it on the slot once the
// running task ends.
func TestQueuedAllocationStaysQueuedAcrossMasterRestart(t *testing.T) {
	database, dropDB := db.MustResolveNewPostgresDatabase(t)
	t.Cleanup(dropDB)
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	owner := db.RequireMockUser(t, database)
	poolName := "restore-" + uuid.NewString()[:8]
	agentID := aproto.ID(poolName + "-agent")
	agentStarted := func(reattached ...aproto.ContainerReattachAck) *aproto.AgentStarted {
		return &aproto.AgentStarted{
			Version:          "test",
			ResourcePoolName: poolName,
			Devices: []device.Device{{
				ID: 0, Brand: "nvda", UUID: "GPU-" + string(agentID), Type: device.CUDA,
			}},
			ContainersReattached: reattached,
		}
	}

	// The first master: a task runs on the agent's only slot and a second one waits for it.
	first := newRestoreTestMaster(t, poolName)
	firstAgent := connectRestoreTestAgent(t, first.addAgent(t, agentID, nil), agentStarted())
	running := newRestoreTestRequest(t, poolName)
	queued := newRestoreTestRequest(t, poolName)
	restoreTestAllocation(t, first, running, &owner)
	runningContainer := firstAgent.awaitStart(running.AllocationID)
	firstAgent.report(runningContainer, cproto.Running)
	requireAllocationState(t, running.AllocationID, model.AllocationStateRunning)
	restoreTestAllocation(t, first, queued, &owner)
	time.Sleep(4 * actionCoolDown)
	requireAllocationState(t, queued.AllocationID, model.AllocationStatePending)

	// The master stops. The second master restores the agent from the database and the
	// allocations that have not ended.
	detach(t, running.AllocationID, queued.AllocationID)
	states, err := retrieveAgentStates()
	require.NoError(t, err)
	state, ok := states[agentID]
	require.True(t, ok)
	second := newRestoreTestMaster(t, poolName)
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

	time.Sleep(4 * actionCoolDown)
	requireAllocationState(t, running.AllocationID, model.AllocationStateRunning)
	requireAllocationState(t, queued.AllocationID, model.AllocationStatePending)
	jobs, err := second.rm.GetJobQ("")
	require.NoError(t, err)
	require.Contains(t, jobs, queued.JobID)
	require.Equal(t, sproto.SchedulingStateQueued, jobs[queued.JobID].State)

	// The agent reconnects with the running task's container. When that task ends, the waiting
	// task starts on the slot.
	secondAgent := connectRestoreTestAgent(t, restoredAgent, agentStarted(aproto.ContainerReattachAck{
		Container: cproto.Container{
			ID: runningContainer.ID, State: cproto.Running, Devices: runningContainer.Devices,
		},
	}))
	secondAgent.report(runningContainer, cproto.Terminated)
	queuedContainer := secondAgent.awaitStart(queued.AllocationID)
	require.Equal(t, runningContainer.Devices, queuedContainer.Devices)
	requireAllocationState(t, queued.AllocationID, model.AllocationStateAssigned)
}
