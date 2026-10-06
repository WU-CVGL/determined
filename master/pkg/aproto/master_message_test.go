package aproto

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// agentStartedV041 is AgentStarted as of 0.41.0, before GPUTopology.
type agentStartedV041 struct {
	Version              string
	Devices              []device.Device
	ContainersReattached []ContainerReattachAck
	ResourcePoolName     string
}

// goldenAgentStartedV041 is how a 0.41.0 agent sends AgentStarted.
const goldenAgentStartedV041 = `{"Version":"0.41.0","Devices":[{"id":0,` +
	`"brand":"NVIDIA GeForce RTX 4090","uuid":"GPU-a","type":"cuda"}],"ContainersReattached":` +
	`[{"Container":{"id":"c1","state":"RUNNING","devices":null,"description":""},"Failure":null}],` +
	`"ResourcePoolName":"default"}`

func TestAgentStartedWireCompat(t *testing.T) {
	devices := []device.Device{{ID: 0, Brand: "NVIDIA GeForce RTX 4090", UUID: "GPU-a", Type: device.CUDA}}
	reattached := []ContainerReattachAck{{Container: cproto.Container{ID: "c1", State: cproto.Running}}}

	// A nil GPUTopology marshals byte-identical to today's message.
	now, err := json.Marshal(AgentStarted{
		Version: "0.41.0", Devices: devices, ContainersReattached: reattached, ResourcePoolName: "default",
	})
	require.NoError(t, err)
	old, err := json.Marshal(agentStartedV041{
		Version: "0.41.0", Devices: devices, ContainersReattached: reattached, ResourcePoolName: "default",
	})
	require.NoError(t, err)
	require.Equal(t, string(old), string(now))
	require.Equal(t, goldenAgentStartedV041, string(now))

	// An older agent's message decodes with a nil GPUTopology, which means unknown.
	var fromOldAgent AgentStarted
	require.NoError(t, json.Unmarshal([]byte(goldenAgentStartedV041), &fromOldAgent))
	require.Nil(t, fromOldAgent.GPUTopology)
	require.Equal(t, devices, fromOldAgent.Devices)

	// An older master decodes a new agent's message with plain json.Unmarshal and ignores the
	// field.
	zero := 0
	collected := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	withTopology, err := json.Marshal(MasterMessage{AgentStarted: &AgentStarted{
		Version: "0.42.0", Devices: devices, ResourcePoolName: "default",
		GPUTopology: &GPUTopology{
			CollectedAt:   &collected,
			DriverVersion: "610.57.04",
			GPUs:          []GPUInfo{{UUID: "GPU-a", PCIBusID: "0000:41:00.0", NUMANode: &zero}},
		},
	}})
	require.NoError(t, err)
	var oldMaster struct{ AgentStarted *agentStartedV041 }
	require.NoError(t, json.Unmarshal(withTopology, &oldMaster))
	require.Equal(t, devices, oldMaster.AgentStarted.Devices)

	// And a new master gets it back.
	var newMaster MasterMessage
	require.NoError(t, json.Unmarshal(withTopology, &newMaster))
	require.Equal(t, "610.57.04", newMaster.AgentStarted.GPUTopology.DriverVersion)
	require.True(t, collected.Equal(*newMaster.AgentStarted.GPUTopology.CollectedAt))
	require.Equal(t, 0, *newMaster.AgentStarted.GPUTopology.GPUs[0].NUMANode)
}

func TestGPUTopologyWireJSON(t *testing.T) {
	two := 2
	bs, err := json.Marshal(GPUTopology{
		DriverVersion: "610.57.04",
		GPUs: []GPUInfo{
			{UUID: "GPU-a", NUMANode: &two, PCIeLinkWidth: 8, PCIeLinkWidthMax: 16},
			{UUID: "GPU-b", Excluded: true},
		},
		Links: []GPULink{{
			UUIDA: "GPU-a", UUIDB: "GPU-b", Level: GPULinkLevelPIX, NVLinks: 4,
			P2PAToB: GPUP2PCaps{Read: GPUP2PStatusOK},
		}},
	})
	require.NoError(t, err)
	// An unknown collection time is left out, as every unknown field is.
	require.NotContains(t, string(bs), "collected_at")
	require.JSONEq(t, `{
		"driver_version": "610.57.04",
		"gpus": [
			{"uuid": "GPU-a", "numa_node": 2, "pcie_link_width": 8, "pcie_link_width_max": 16},
			{"uuid": "GPU-b", "excluded": true}
		],
		"links": [{
			"uuid_a": "GPU-a", "uuid_b": "GPU-b", "level": "PIX", "nvlinks": 4,
			"p2p_a_to_b": {"read": "OK"}, "p2p_b_to_a": {}
		}]
	}`, string(bs))
}
