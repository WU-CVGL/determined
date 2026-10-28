//go:build integration
// +build integration

package agentrm

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ws"
)

// gpuOwnersHarness is an agent service with one pool. Its agents connect over a test websocket,
// since the service's own websocket handler needs the master's connection multiplexer.
type gpuOwnersHarness struct {
	t       *testing.T
	pool    string
	service *agents
}

func newGPUOwnersHarness(t *testing.T) *gpuOwnersHarness {
	poolName := "gpu-owners-" + uuid.NewString()[:8]
	poolConfig := config.ResourcePoolConfig{
		PoolName:                 poolName,
		MaxAuxContainersPerAgent: 100,
		AgentReconnectWait:       model.Duration(time.Minute),
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
		ResourcePools: []config.ResourcePoolConfig{poolConfig},
	}
	registry, err := newPoolRegistry(rmConfig.ResourcePools)
	require.NoError(t, err)
	opts := &aproto.MasterSetAgentOptions{
		LoggingOptions: model.LoggingConfig{DefaultLoggingConfig: &model.DefaultLoggingConfig{}},
	}
	service, agentUpdates := newAgentService(registry, opts, false)
	rm, err := newAgentResourceManager(nil, rmConfig, nil, service, agentUpdates, registry)
	require.NoError(t, err)
	t.Cleanup(rm.stop)
	return &gpuOwnersHarness{t: t, pool: poolName, service: service}
}

// register creates and registers an agent as the service does for a new websocket, and returns
// it with the URL a fake agent connects to.
func (h *gpuOwnersHarness) register(id aproto.ID) (*agent, string) {
	var ref *agent
	ref, err := h.service.createAgent(id, h.pool, h.service.opts, nil, func() {
		tasklist.DeleteIf(h.service.agents, id, ref)
	})
	require.NoError(h.t, err)
	require.NoError(h.t, h.service.agents.Add(id, ref))

	e := echo.New()
	e.GET("/", func(c echo.Context) error {
		require.NoError(h.t, ref.HandleWebsocketConnection(webSocketRequest{echoCtx: c}))
		return nil
	})
	server := httptest.NewServer(e.Server.Handler)
	h.t.Cleanup(server.Close)
	h.t.Cleanup(func() {
		ref.mu.Lock()
		defer ref.mu.Unlock()
		ref.stop(nil)
	})
	return ref, fmt.Sprintf("ws://%s", strings.TrimPrefix(server.URL, "http://"))
}

// connect opens the fake agent's websocket, closed at the end of the test, and sends AgentStarted
// with the given GPU UUIDs.
func (h *gpuOwnersHarness) connect(
	url string, uuids ...string,
) (*websocket.Conn, *ws.WebSocket[aproto.AgentMessage, *aproto.MasterMessage]) {
	var dialer websocket.Dialer
	conn, _, err := dialer.Dial(url, nil)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = conn.Close() })
	socket, err := ws.Wrap[aproto.AgentMessage, *aproto.MasterMessage]("test", conn)
	require.NoError(h.t, err)
	started := &aproto.AgentStarted{Version: "test", ResourcePoolName: h.pool}
	for i, u := range uuids {
		started.Devices = append(started.Devices, device.Device{
			ID: device.ID(i), Brand: "test", UUID: u, Type: device.CUDA,
		})
	}
	socket.Outbox <- &aproto.MasterMessage{AgentStarted: started}
	return conn, socket
}

func (h *gpuOwnersHarness) waitFor(a *agent, cond func() bool, what string) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		ok := cond()
		a.mu.Unlock()
		if ok {
			return
		}
		require.True(h.t, time.Now().Before(deadline), "timed out waiting for %s", what)
		time.Sleep(10 * time.Millisecond)
	}
}

// shutdownMessage returns the ErrMsg of the AgentShutdown the master sends on socket.
func (h *gpuOwnersHarness) shutdownMessage(
	socket *ws.WebSocket[aproto.AgentMessage, *aproto.MasterMessage],
) string {
	timeout := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-socket.Inbox:
			require.True(h.t, ok, "socket closed before AgentShutdown")
			if msg.AgentShutdown != nil {
				return msg.AgentShutdown.ErrMsg
			}
		case <-timeout:
			require.FailNow(h.t, "timed out waiting for AgentShutdown")
		}
	}
}

func TestAgentWithGPUOfAnotherAgentIsRefused(t *testing.T) {
	h := newGPUOwnersHarness(t)
	suffix := uuid.NewString()[:8]
	gpuA, gpuB, gpuC := "GPU-a-"+suffix, "GPU-b-"+suffix, "GPU-c-"+suffix
	firstID, secondID := aproto.ID("node01-"+suffix), aproto.ID("node09-"+suffix)

	first, firstURL := h.register(firstID)
	firstConn, _ := h.connect(firstURL, gpuA, gpuB)
	h.waitFor(first, func() bool { return first.started }, "the first agent to start")

	// A second agent ID that reports a GPU of the first agent is refused and never added. It is
	// stopped when it disconnects on the shutdown message.
	second, secondURL := h.register(secondID)
	secondConn, secondSocket := h.connect(secondURL, gpuC, gpuB)
	require.Equal(t,
		fmt.Sprintf("agent %s refused: GPU %s is already reported by agent %s",
			secondID, gpuB, firstID),
		h.shutdownMessage(secondSocket))
	second.mu.Lock()
	require.False(t, second.started)
	second.mu.Unlock()
	require.Len(t, h.service.list(h.pool), 1)
	require.NoError(t, secondConn.Close())
	h.waitFor(second, func() bool { return second.stopped }, "the second agent to stop")
	_, ok := h.service.agents.Load(secondID)
	require.False(t, ok, "the refused agent is still registered")
	require.Len(t, h.service.list(h.pool), 1)

	// The first agent reconnects with the same ID and devices: accepted.
	require.NoError(t, firstConn.UnderlyingConn().Close())
	h.waitFor(first, func() bool { return first.awaitingReconnect }, "the disconnect")
	first.mu.Lock()
	topology := first.agentState.gpuTopology
	first.mu.Unlock()
	h.connect(firstURL, gpuA, gpuB)
	h.waitFor(first, func() bool { return first.agentState.gpuTopology != topology },
		"the AgentStarted of the reconnect")
	first.mu.Lock()
	require.True(t, first.started)
	require.False(t, first.stopped)
	require.False(t, first.awaitingReconnect)
	first.mu.Unlock()

	// Once the first agent is gone, another agent ID may report its GPUs.
	first.mu.Lock()
	first.stop(nil)
	first.mu.Unlock()
	third, thirdURL := h.register(secondID)
	h.connect(thirdURL, gpuB)
	h.waitFor(third, func() bool { return third.started }, "the third agent to start")
	third.mu.Lock()
	require.False(t, third.stopped)
	third.mu.Unlock()
}

func TestRefusedAgentThatStaysConnectedIsStopped(t *testing.T) {
	wait := refusedAgentStopWait
	refusedAgentStopWait = 200 * time.Millisecond
	t.Cleanup(func() { refusedAgentStopWait = wait })

	h := newGPUOwnersHarness(t)
	suffix := uuid.NewString()[:8]
	gpu := "GPU-" + suffix

	first, firstURL := h.register(aproto.ID("node01-" + suffix))
	h.connect(firstURL, gpu)
	h.waitFor(first, func() bool { return first.started }, "the first agent to start")

	second, secondURL := h.register(aproto.ID("node09-" + suffix))
	_, secondSocket := h.connect(secondURL, gpu)
	require.Contains(t, h.shutdownMessage(secondSocket), "already reported by agent node01-"+suffix)
	h.waitFor(second, func() bool { return second.stopped }, "the second agent to stop")
	_, ok := h.service.agents.Load(second.id)
	require.False(t, ok, "the refused agent is still registered")
}

// restoredGPUTest is a master that ran a one-GPU task on agent A, which has two GPUs, and was
// restarted: the new master has restored A from the database and the task's allocation.
type restoredGPUTest struct {
	master    *restoreTestMaster
	restored  *agent
	running   sproto.AllocateRequest
	container cproto.Container
	owner     model.User
	gpus      []string
}

func (r *restoredGPUTest) agentStarted(reattached ...aproto.ContainerReattachAck) *aproto.AgentStarted {
	started := &aproto.AgentStarted{
		Version:              "test",
		ResourcePoolName:     r.master.pool.PoolName,
		ContainersReattached: reattached,
	}
	for i, u := range r.gpus {
		started.Devices = append(started.Devices,
			device.Device{ID: device.ID(i), Brand: "nvda", UUID: u, Type: device.CUDA})
	}
	return started
}

// reattached is the running task's container as the agent reports it on reconnect.
func (r *restoredGPUTest) reattached() aproto.ContainerReattachAck {
	return aproto.ContainerReattachAck{Container: cproto.Container{
		ID: r.container.ID, State: cproto.Running, Devices: r.container.Devices,
	}}
}

func newRestoredGPUTest(t *testing.T, poolName string, agentID aproto.ID) *restoredGPUTest {
	database, dropDB := db.MustResolveNewPostgresDatabase(t)
	t.Cleanup(dropDB)
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	r := &restoredGPUTest{
		owner: db.RequireMockUser(t, database),
		gpus:  []string{"GPU-0-" + poolName, "GPU-1-" + poolName},
	}

	first := newRestoreTestMaster(t, poolName, false)
	r.master = first
	firstAgent := connectRestoreTestAgent(t, first.addAgent(t, agentID, nil), r.agentStarted())
	r.running = newRestoreTestRequest(t, poolName)
	restoreTestAllocation(t, first, r.running, &r.owner)
	r.container = firstAgent.awaitStart(r.running.AllocationID)
	firstAgent.report(r.container, cproto.Running)
	requireAllocationState(t, r.running.AllocationID, model.AllocationStateRunning)

	detach(t, r.running.AllocationID)
	r.master = newRestoreTestMaster(t, poolName, true)
	restored, ok := r.master.agents.get(agentID)
	require.True(t, ok, "agent not restored")
	r.restored = restored
	r.running.Restore = true
	restoreTestAllocation(t, r.master, r.running, &r.owner)
	t.Cleanup(func() {
		if slices.Contains(task.DefaultService.GetAllAllocationIDs(), r.running.AllocationID) {
			detach(t, r.running.AllocationID)
		}
	})
	return r
}

// After a master restart, an agent with another ID that reports the GPU of a restored agent is
// refused while the restored agent awaits its reconnect. The restored agent then reconnects with
// its task.
func TestRestoredAgentKeepsItsGPUs(t *testing.T) {
	poolName := "gpu-restore-" + uuid.NewString()[:8]
	agentID, otherID := aproto.ID(poolName+"-a"), aproto.ID(poolName+"-b")
	r := newRestoredGPUTest(t, poolName, agentID)

	other := connectRestoreTestAgent(t, r.master.registerAgent(t, otherID), r.agentStarted())
	require.Equal(t,
		fmt.Sprintf("agent %s refused: GPU %s is already reported by agent %s",
			otherID, strings.Join(r.gpus, ","), agentID),
		other.awaitShutdown())
	require.NoError(t, other.socket.Close())
	require.Eventually(t, func() bool {
		_, ok := r.master.agents.get(otherID)
		return !ok
	}, 10*time.Second, 10*time.Millisecond, "the refused agent is still registered")
	other.agent.mu.Lock()
	require.False(t, other.agent.started)
	other.agent.mu.Unlock()

	connectRestoreTestAgent(t, r.restored, r.agentStarted(r.reattached()))
	require.Eventually(t, func() bool {
		r.restored.mu.Lock()
		defer r.restored.mu.Unlock()
		return !r.restored.awaitingReconnect
	}, 10*time.Second, 10*time.Millisecond, "the restored agent never recovered")
	time.Sleep(4 * actionCoolDown)
	r.restored.mu.Lock()
	require.False(t, r.restored.stopped)
	require.True(t, r.restored.agentState.enabled)
	require.Equal(t, r.running.AllocationID, r.restored.agentState.containerAllocation[r.container.ID])
	r.restored.mu.Unlock()
	requireAllocationState(t, r.running.AllocationID, model.AllocationStateRunning)
	_, ok := r.master.agents.get(agentID)
	require.True(t, ok)
}

// A restored agent whose websocket connects stays disabled, and gets no container, until its
// AgentStarted passes the checks.
func TestRestoredAgentWaitsForAgentStarted(t *testing.T) {
	poolName := "gpu-restore-" + uuid.NewString()[:8]
	r := newRestoredGPUTest(t, poolName, aproto.ID(poolName+"-a"))

	// A task that waits for the free GPU, and a start that was buffered while the agent was away.
	queued := newRestoreTestRequest(t, poolName)
	restoreTestAllocation(t, r.master, queued, &r.owner)
	buffered := cproto.ID("buffered-" + uuid.NewString()[:8])
	r.restored.StartTaskContainer(sproto.StartTaskContainer{
		AllocationID:   model.AllocationID("buffered-" + uuid.NewString()[:8]),
		StartContainer: aproto.StartContainer{Container: cproto.Container{ID: buffered}},
	})

	ta := dialRestoreTestAgent(t, r.restored)
	ta.requireNoStart(4 * actionCoolDown)
	requireAllocationState(t, queued.AllocationID, model.AllocationStatePending)
	r.restored.mu.Lock()
	require.True(t, r.restored.awaitingReconnect)
	require.False(t, r.restored.agentState.enabled)
	require.Len(t, r.restored.reconnectBacklog, 1)
	r.restored.mu.Unlock()

	ta.socket.Outbox <- &aproto.MasterMessage{AgentStarted: r.agentStarted(r.reattached())}
	queuedContainer := ta.awaitStart(queued.AllocationID)
	require.NotEqual(t, r.container.Devices, queuedContainer.Devices)
	requireAllocationState(t, r.running.AllocationID, model.AllocationStateRunning)
	r.restored.mu.Lock()
	require.False(t, r.restored.awaitingReconnect)
	require.True(t, r.restored.agentState.enabled)
	require.Empty(t, r.restored.reconnectBacklog)
	r.restored.mu.Unlock()
}
