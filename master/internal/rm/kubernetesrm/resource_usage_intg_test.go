//go:build integration

package kubernetesrm

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/userusage"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
)

// New and restored Kubernetes job resources take every field from the one constructor, and their
// devices carry the resource manager's slot type, one per slot of each pod.
func TestK8sJobResourceSummarySlotType(t *testing.T) {
	jobID := model.JobID("job")
	req := &sproto.AllocateRequest{AllocationID: model.AllocationID("a.1"), JobID: jobID}
	group := &tasklist.Group{JobID: jobID}
	for _, slotType := range []device.Type{device.CUDA, device.CPU} {
		jobs := &jobsService{slotType: slotType}
		k := &kubernetesResourcePool{
			jobsService:         jobs,
			groups:              map[model.JobID]*tasklist.Group{jobID: group},
			allocationIDToJobID: map[model.AllocationID]model.JobID{req.AllocationID: jobID},
			queuePositions:      tasklist.JobSortState{jobID: decimal.NewFromInt(7)},
			defaultNamespace:    "namespace",
			clusterName:         "cluster",
		}
		require.Equal(t, k.newJobResource(req, 4, 2, nil), k.createResources(req, 4, 2))
		// Restored resources that had started.
		for _, started := range []*sproto.ResourcesStarted{nil, {}} {
			got := k.newJobResource(req, 4, 2, started)
			require.Equal(t, &k8sJobResource{
				req:              req,
				jobsService:      jobs,
				group:            group,
				slots:            4,
				numPods:          2,
				slotType:         slotType,
				initialPosition:  decimal.NewFromInt(7),
				defaultNamespace: "namespace",
				clusterName:      "cluster",
				started:          started,
			}, got)

			devices := got.Summary().AgentDevices["pods"]
			require.Len(t, devices, 8)
			for _, d := range devices {
				require.Equal(t, slotType, d.Type)
			}
		}
	}

	k := &kubernetesResourcePool{jobsService: &jobsService{slotType: device.CUDA}}
	kind, quantity := userusage.Assigned([]sproto.ResourcesSummary{
		k.newJobResource(req, 2, 3, &sproto.ResourcesStarted{}).Summary(),
	}, 6)
	require.Equal(t, userusage.KindGPU, kind)
	require.Equal(t, 6, quantity)

	k = &kubernetesResourcePool{jobsService: &jobsService{slotType: device.CPU}}
	kind, quantity = userusage.Assigned([]sproto.ResourcesSummary{
		k.newJobResource(req, 0, 1, nil).Summary(),
	}, 0)
	require.Equal(t, userusage.KindNone, kind)
	require.Zero(t, quantity)
}
