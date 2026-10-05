//go:build integration
// +build integration

package agentrm

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/syncx/queue"
	"github.com/determined-ai/determined/master/pkg/ws"
)

// gpuTopologyTestAgent serves one agent over a websocket and connects to it as an agent would.
type gpuTopologyTestAgent struct {
	t   *testing.T
	a   *agent
	url string
}

func newGPUTopologyTestAgent(t *testing.T) *gpuTopologyTestAgent {
	a := newAgent(
		aproto.ID("gpu-topology-"+time.Now().Format("150405.000000")),
		queue.New[agentUpdatedEvent](),
		"default",
		&config.ResourcePoolConfig{AgentReconnectWait: model.Duration(time.Minute)},
		&aproto.MasterSetAgentOptions{
			LoggingOptions: model.LoggingConfig{DefaultLoggingConfig: &model.DefaultLoggingConfig{}},
		},
		nil,
		func() {},
	)
	e := echo.New()
	e.GET("/", func(c echo.Context) error {
		require.NoError(t, a.HandleWebsocketConnection(webSocketRequest{echoCtx: c}))
		return nil
	})
	server := httptest.NewServer(e.Server.Handler)
	t.Cleanup(server.Close)
	return &gpuTopologyTestAgent{
		t: t, a: a, url: fmt.Sprintf("ws://%s", strings.TrimPrefix(server.URL, "http://")),
	}
}

func (g *gpuTopologyTestAgent) connect(started *aproto.AgentStarted) *websocket.Conn {
	var dialer websocket.Dialer
	conn, _, err := dialer.Dial(g.url, nil)
	require.NoError(g.t, err)
	socket, err := ws.Wrap[aproto.AgentMessage, *aproto.MasterMessage]("test", conn)
	require.NoError(g.t, err)
	socket.Outbox <- &aproto.MasterMessage{AgentStarted: started}
	return conn
}

func (g *gpuTopologyTestAgent) waitFor(what string, cond func(a *agent) bool) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.a.mu.Lock()
		ok := cond(g.a)
		g.a.mu.Unlock()
		if ok {
			return
		}
		require.True(g.t, time.Now().Before(deadline), "timed out waiting for %s", what)
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAgentStartedRefreshesGPUTopology: every AgentStarted replaces the topology, on a fresh
// registration and on a reconnect with the same devices (section 4.2).
func TestAgentStartedRefreshesGPUTopology(t *testing.T) {
	g := newGPUTopologyTestAgent(t)
	devices := []device.Device{
		{ID: 0, Brand: "test", UUID: "GPU-a", Type: device.CUDA},
		{ID: 1, Brand: "test", UUID: "GPU-b", Type: device.CUDA},
	}
	started := func(version string, width int) *aproto.AgentStarted {
		msg := &aproto.AgentStarted{Version: version, Devices: devices, ResourcePoolName: "default"}
		if width > 0 {
			collected := time.Now()
			msg.GPUTopology = &aproto.GPUTopology{
				CollectedAt: &collected,
				GPUs: []aproto.GPUInfo{
					{UUID: "GPU-a", PCIeLinkWidth: width, PCIeLinkWidthMax: 16},
					{UUID: "GPU-b", PCIeLinkWidth: 16, PCIeLinkWidthMax: 16},
					{UUID: "GPU-x", PCIBusID: "0000:81:00.0", Excluded: true},
				},
			}
		}
		return msg
	}
	reported := func(width int) func(a *agent) bool {
		return func(a *agent) bool {
			topo := a.agentState
			return a.started && !a.awaitingReconnect && topo != nil && topo.gpuTopology != nil &&
				topo.gpuTopology.gpus[0].PCIeLinkWidth == width
		}
	}

	// A fresh registration.
	first := g.connect(started("0.42.0", 8))
	g.waitFor("the first AgentStarted", reported(8))
	g.a.mu.Lock()
	topo := g.a.agentState.gpuTopology
	require.Empty(t, topo.unknownReason)
	require.Len(t, topo.excluded, 1)
	g.a.mu.Unlock()

	// The API sees it: Summarize fills gpu_topology from the agent state (section 5.2). It takes
	// a.mu itself.
	api := g.a.Summarize().GPUTopology
	require.NotNil(t, api)
	require.Empty(t, api.UnknownReason)
	require.NotNil(t, api.CollectedAt)
	require.Len(t, api.Gpus, 3)
	require.Equal(t, int32(0), api.Gpus[0].DeviceId)
	require.Equal(t, "GPU-a", api.Gpus[0].Uuid)
	require.Equal(t, int32(8), api.Gpus[0].PcieLinkWidth)
	require.Equal(t, int32(1), api.Gpus[1].DeviceId)
	require.Equal(t, "GPU-b", api.Gpus[1].Uuid)
	require.Equal(t, int32(-1), api.Gpus[2].DeviceId)
	require.Equal(t, "GPU-x", api.Gpus[2].Uuid)
	require.True(t, api.Gpus[2].Excluded)

	// A reconnect with the same devices refreshes it.
	require.NoError(t, first.UnderlyingConn().Close())
	g.waitFor("the disconnect", func(a *agent) bool { return a.awaitingReconnect })
	second := g.connect(started("0.42.0", 16))
	g.waitFor("the AgentStarted after the reconnect", reported(16))
	g.a.mu.Lock()
	require.NotSame(t, topo, g.a.agentState.gpuTopology, "a new report replaces the pointer")
	require.Equal(t, 8, topo.gpus[0].PCIeLinkWidth, "the old value is never mutated")
	require.False(t, g.a.stopped)
	g.a.mu.Unlock()
	require.Equal(t, int32(16), g.a.Summarize().GPUTopology.Gpus[0].PcieLinkWidth)

	// An older agent image, reconnecting with the same devices, sends no topology.
	require.NoError(t, second.UnderlyingConn().Close())
	g.waitFor("the second disconnect", func(a *agent) bool { return a.awaitingReconnect })
	third := g.connect(started("0.41.0", 0))
	defer func() { _ = third.Close() }()
	g.waitFor("the AgentStarted of the older agent", func(a *agent) bool {
		return !a.awaitingReconnect && a.agentState.gpuTopology != nil &&
			a.agentState.gpuTopology.unknownReason != ""
	})
	api = g.a.Summarize().GPUTopology
	require.NotNil(t, api)
	require.Equal(t, "agent 0.41.0 does not report GPU topology", api.UnknownReason)
	require.Len(t, api.Gpus, 2, "the two slots, with no telemetry")
	require.Equal(t, int32(0), api.Gpus[0].PcieLinkWidth)

	g.a.mu.Lock()
	defer g.a.mu.Unlock()
	require.Equal(t, "agent 0.41.0 does not report GPU topology", g.a.agentState.gpuTopology.unknownReason)
	require.Empty(t, g.a.agentState.gpuTopology.excluded)
	require.False(t, g.a.stopped)
}
