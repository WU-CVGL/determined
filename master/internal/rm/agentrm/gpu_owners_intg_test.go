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

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/pkg/aproto"
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
