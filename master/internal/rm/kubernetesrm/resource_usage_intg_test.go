//go:build integration

package kubernetesrm

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/userusage"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
)

// A Kubernetes job's devices carry the resource manager's slot type, one per slot of each pod.
func TestK8sJobResourceSummarySlotType(t *testing.T) {
	req := &sproto.AllocateRequest{AllocationID: model.AllocationID("a.1")}
	for _, slotType := range []device.Type{device.CUDA, device.CPU} {
		k := &kubernetesResourcePool{
			jobsService:         &jobsService{slotType: slotType},
			allocationIDToJobID: map[model.AllocationID]model.JobID{},
		}
		summary := k.createResources(req, 4, 2).Summary()
		devices := summary.AgentDevices["pods"]
		require.Len(t, devices, 8)
		for _, d := range devices {
			require.Equal(t, slotType, d.Type)
		}
	}

	// The fields that the restore sets, as createResources does.
	restored := k8sJobResource{req: req, slots: 2, numPods: 3, slotType: device.CUDA}
	kind, quantity := userusage.Assigned([]sproto.ResourcesSummary{restored.Summary()}, 6)
	require.Equal(t, userusage.KindGPU, kind)
	require.Equal(t, 6, quantity)

	kind, quantity = userusage.Assigned([]sproto.ResourcesSummary{
		(k8sJobResource{req: req, numPods: 1, slotType: device.CPU}).Summary(),
	}, 0)
	require.Equal(t, userusage.KindNone, kind)
	require.Zero(t, quantity)
}
