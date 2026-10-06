package model

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

func TestSortableSlotIndex(t *testing.T) {
	require.Equal(t, "002", SortableSlotIndex(2))
	require.Equal(t, "016", SortableSlotIndex(16))

	// Do we actually sort?
	var gpuIndexes []string
	for i := 0; i <= 999; i++ {
		gpuIndexes = append(gpuIndexes, SortableSlotIndex(i))
	}
	require.True(t, slices.IsSorted(gpuIndexes))
}

func TestAgentSummaryToProtoGPUTopology(t *testing.T) {
	topo := &agentv1.GpuTopology{
		DriverVersion: "610.57.04",
		Gpus:          []*agentv1.GpuInfo{{DeviceId: -1, Uuid: "GPU-x", Excluded: true}},
	}
	require.Same(t, topo, AgentSummary{ID: "a", GPUTopology: topo}.ToProto().GpuTopology)
	require.Nil(t, AgentSummary{ID: "cpu"}.ToProto().GpuTopology)
}
