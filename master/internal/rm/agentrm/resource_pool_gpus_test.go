package agentrm

import (
	"context"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/gpuhealth"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

func TestPoolGPUPolicy(t *testing.T) {
	for name, c := range map[string]struct {
		scheduler config.SchedulerConfig
		packs     bool
	}{
		"best":             {config.SchedulerConfig{FittingPolicy: best}, true},
		"best, switch on":  {config.SchedulerConfig{FittingPolicy: best, NUMAPacking: ptrs.Ptr(true)}, true},
		"best, switch off": {config.SchedulerConfig{FittingPolicy: best, NUMAPacking: ptrs.Ptr(false)}, false},
		"worst":            {config.SchedulerConfig{FittingPolicy: worst}, false},
	} {
		rp := &resourcePool{config: &config.ResourcePoolConfig{Scheduler: &c.scheduler}}
		policy := rp.newGPUPolicy()
		require.Equal(t, c.packs, policy.packNUMA, name)

		req := &sproto.AllocateRequest{SlotsNeeded: 2}
		sel := policy.selection(req, []*fittingState{{Slots: 2}})
		require.Equal(t, c.packs, sel.packNUMA, name)
		// A multi-agent fit packs too: it takes the free devices the fit counted.
		sel = policy.selection(req, []*fittingState{{Slots: 1}, {Slots: 1}})
		require.Equal(t, c.packs, sel.packNUMA, name)
	}
}

func TestGPUXIDReaderRecent(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	snapshot := &gpuhealth.XIDSnapshot{
		Status: agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK, QueriedAt: now.Add(-time.Hour),
		ByUUID: map[string][]gpuhealth.XID{
			"GPU-recent":  {{Code: 79, FirstObserved: now.Add(-2 * time.Hour), LastObserved: now.Add(-time.Hour)}},
			"GPU-expired": {{Code: 79, LastObserved: now.Add(-gpuhealth.XIDWindow - time.Minute)}},
			// The query leaves application codes out; they never count here either.
			"GPU-app": {{Code: 13, LastObserved: now.Add(-time.Minute)}},
		},
	}

	var r *gpuXIDReader
	require.Nil(t, r.recent(now), "a pool without a reader")
	r = &gpuXIDReader{}
	require.Nil(t, r.recent(now), "before the master sets it")
	r.set(func() *gpuhealth.XIDSnapshot { return nil })
	require.Nil(t, r.recent(now), "no successful query yet")
	r.set(func() *gpuhealth.XIDSnapshot { return snapshot })
	require.Equal(t, map[string]bool{"GPU-recent": true}, r.recent(now))
}

func TestSetGPUXIDsReachesThePools(t *testing.T) {
	conf := &config.ResourceConfig{
		RootManagerInternal: &config.ResourceManagerConfig{
			AgentRM: &config.AgentResourceManagerConfig{
				Scheduler: &config.SchedulerConfig{Priority: &config.PrioritySchedulerConfig{
					DefaultPriority: ptrs.Ptr(42),
				}, FittingPolicy: best},
			},
		},
		RootPoolsInternal: []config.ResourcePoolConfig{
			{PoolName: defaultResourcePoolName, MaxAuxContainersPerAgent: 100},
		},
	}
	rm, err := New(context.Background(), nil, echo.New(), conf.ResourceManagers()[0], nil, nil, nil)
	require.NoError(t, err)
	defer rm.stop()
	pool, err := rm.poolByName(defaultResourcePoolName)
	require.NoError(t, err)

	rm.SetGPUXIDs(func() *gpuhealth.XIDSnapshot {
		return &gpuhealth.XIDSnapshot{ByUUID: map[string][]gpuhealth.XID{
			"GPU-3": {{Code: 48, LastObserved: time.Now()}},
		}}
	})
	pool.mu.Lock()
	policy := pool.newGPUPolicy()
	pool.mu.Unlock()
	require.Equal(t, map[string]bool{"GPU-3": true}, policy.xids)
	require.True(t, policy.packNUMA)
}
