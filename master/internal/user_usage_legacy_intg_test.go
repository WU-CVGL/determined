//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/devicev1"
	"github.com/determined-ai/determined/proto/pkg/resourcepoolv1"
)

// The allocations from before metering take their pool's kind once the resource manager reports
// it, and keep it whatever the pool reports later.
func TestUserUsageLegacyClassifier(t *testing.T) {
	ctx := context.Background()
	api, _, _ := setupAPITest(t, nil)
	suffix := uuid.NewString()[:8]
	static, k8s, gone := "static-"+suffix, "k8s-"+suffix, "gone-"+suffix

	type rows struct{ zero, slots model.AllocationID }
	legacy := func(pool string) rows {
		add := func(slots int) model.AllocationID {
			task := db.RequireMockTask(t, api.m.db, nil)
			a := model.Allocation{
				AllocationID: model.AllocationID(fmt.Sprintf("%s.0", task.TaskID)),
				TaskID:       task.TaskID,
				Slots:        slots,
				ResourcePool: pool,
				MeteredKind:  ptrs.Ptr("unclassified"),
			}
			require.NoError(t, db.AddAllocation(ctx, &a))
			return a.AllocationID
		}
		return rows{zero: add(0), slots: add(2)}
	}
	staticRows, k8sRows, goneRows := legacy(static), legacy(k8s), legacy(gone)

	requireKind := func(id model.AllocationID, kind string, quantity *int) {
		a, err := db.AllocationByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, &kind, a.MeteredKind, "allocation %s", id)
		if quantity != nil {
			require.Equal(t, quantity, a.MeteredQuantity, "allocation %s", id)
		}
	}

	var rm mocks.ResourceManager
	report := func(staticType devicev1.Type, agents int32) {
		rm.On("GetResourcePools").Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{
				{
					Name: static, Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC,
					SlotType: staticType, NumAgents: agents,
				},
				{
					Name: k8s, Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S,
					SlotType: devicev1.Type_TYPE_CUDA,
				},
			},
		}, nil).Once()
	}
	c := &legacyClassifier{
		rm: &rm, agentOnly: true, stableTicks: 2, pools: map[string]*legacyPoolObservation{},
	}

	// No agent connected: the pools' types are known, the static pool's slot type is not.
	report(devicev1.Type_TYPE_UNSPECIFIED, 0)
	_, err := c.step(ctx)
	require.NoError(t, err)
	requireKind(staticRows.zero, "aux", ptrs.Ptr(1))
	requireKind(staticRows.slots, "unclassified", nil)
	requireKind(k8sRows.zero, "none", ptrs.Ptr(0))
	requireKind(k8sRows.slots, "gpu", ptrs.Ptr(2))
	// A pool that is no longer reported was an agent pool on a cluster of agent pools only.
	requireKind(goneRows.zero, "aux", ptrs.Ptr(1))
	requireKind(goneRows.slots, "unclassified", nil)

	// CPU agents connect; the slot type counts once it is stable.
	report(devicev1.Type_TYPE_CPU, 1)
	_, err = c.step(ctx)
	require.NoError(t, err)
	requireKind(staticRows.slots, "unclassified", nil)
	report(devicev1.Type_TYPE_CPU, 1)
	_, err = c.step(ctx)
	require.NoError(t, err)
	requireKind(staticRows.slots, "cpu", ptrs.Ptr(2))

	// Neither agents going away nor a later slot type change a classified allocation.
	report(devicev1.Type_TYPE_UNSPECIFIED, 0)
	_, err = c.step(ctx)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		report(devicev1.Type_TYPE_CUDA, 2)
		_, err = c.step(ctx)
		require.NoError(t, err)
	}
	requireKind(staticRows.slots, "cpu", ptrs.Ptr(2))
	requireKind(k8sRows.slots, "gpu", ptrs.Ptr(2))
	requireKind(goneRows.slots, "unclassified", nil)

	// Without agent pools only, the allocations of a pool no longer reported stay unclassified.
	other := legacy("gone-other-" + suffix)
	c.agentOnly = false
	report(devicev1.Type_TYPE_CUDA, 2)
	_, err = c.step(ctx)
	require.NoError(t, err)
	requireKind(other.zero, "unclassified", nil)
	requireKind(other.slots, "unclassified", nil)
}
