package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/proto/pkg/devicev1"
	"github.com/determined-ai/determined/proto/pkg/resourcepoolv1"
)

// A static pool's slot type is known once it was reported the same for ten minutes.
func TestUserUsageLegacyStableTime(t *testing.T) {
	require.Equal(t, 10*time.Minute, (legacyStableTicks-1)*legacyClassifyInterval)
}

func legacyPool(
	typ resourcepoolv1.ResourcePoolType, slotType devicev1.Type, agents int32,
) *resourcepoolv1.ResourcePool {
	return &resourcepoolv1.ResourcePool{Name: "p", Type: typ, SlotType: slotType, NumAgents: agents}
}

// A static pool's slot type counts once it is reported the same, with the same number of agents,
// for the given number of steps; pools whose slot type is configured count at once.
func TestUserUsageLegacyStableSlotType(t *testing.T) {
	static := resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC
	unspecified, cpu, cuda := devicev1.Type_TYPE_UNSPECIFIED, devicev1.Type_TYPE_CPU,
		devicev1.Type_TYPE_CUDA
	type step struct {
		pool     *resourcepoolv1.ResourcePool
		slotType device.Type
		known    bool
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"stable", []step{
			{legacyPool(static, cuda, 2), "", false},
			{legacyPool(static, cuda, 2), "", false},
			{legacyPool(static, cuda, 2), device.CUDA, true},
		}},
		{"no agents yet", []step{
			{legacyPool(static, unspecified, 0), "", false},
			{legacyPool(static, cpu, 1), "", false},
			{legacyPool(static, cpu, 1), "", false},
			{legacyPool(static, cpu, 1), device.CPU, true},
		}},
		{"cpu on a subset of the agents, then cuda with more", []step{
			{legacyPool(static, cpu, 1), "", false},
			{legacyPool(static, cuda, 3), "", false},
			{legacyPool(static, cuda, 3), "", false},
			{legacyPool(static, cuda, 3), device.CUDA, true},
		}},
		{"an agent that never comes back", []step{
			{legacyPool(static, cuda, 2), "", false},
			{legacyPool(static, cuda, 2), "", false},
			{legacyPool(static, cuda, 1), "", false},
			{legacyPool(static, cuda, 1), "", false},
			{legacyPool(static, cuda, 1), device.CUDA, true},
		}},
		{"agents that keep joining", []step{
			{legacyPool(static, cuda, 1), "", false},
			{legacyPool(static, cuda, 2), "", false},
			{legacyPool(static, cuda, 3), "", false},
			{legacyPool(static, cuda, 3), "", false},
			{legacyPool(static, cuda, 3), device.CUDA, true},
		}},
		{"mixed agents", []step{
			{legacyPool(static, unspecified, 2), "", false},
			{legacyPool(static, unspecified, 2), "", false},
			{legacyPool(static, unspecified, 2), "", false},
		}},
		{"aws", []step{
			{legacyPool(resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_AWS, cuda, 0), device.CUDA, true},
		}},
		{"gcp", []step{
			{legacyPool(resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_GCP, cpu, 0), device.CPU, true},
		}},
		{"k8s", []step{
			{legacyPool(resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S, cuda, 0), device.CUDA, true},
		}},
		{"k8s unspecified", []step{
			{legacyPool(resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S, unspecified, 0), "", false},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &legacyClassifier{stableTicks: 3, pools: map[string]*legacyPoolObservation{}}
			for i, s := range tc.steps {
				c.observe(s.pool)
				slotType, known := c.slotType(s.pool)
				require.Equal(t, s.known, known, "step %d", i)
				require.Equal(t, s.slotType, slotType, "step %d", i)
			}
		})
	}
}
