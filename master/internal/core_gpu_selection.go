package internal

import (
	"context"
	"time"

	"github.com/determined-ai/determined/master/internal/gpuhealth"
	"github.com/determined-ai/determined/master/internal/rm/agentrm"
)

// gpuXIDRefreshEvery is how often the master asks the XID cache for the XIDs that GPU selection
// reads: just over the cache's TTL, so that each tick finds the last result expired and starts one
// query. At exactly the TTL, a tick finds the result a little younger than the TTL every other
// time, and the result is refreshed only about once a minute.
const gpuXIDRefreshEvery = gpuhealth.XIDCacheTTL + time.Second

// shareGPUXIDs gives the agent resource managers the GPUs' recent critical XIDs for GPU selection,
// which ranks a GPU with one last. Selection never queries: it reads the last successful result
// (XIDCache.LastOK). With a Prometheus (integrations.task_resources), a background loop keeps that
// result current until ctx ends, asking the cache every gpuXIDRefreshEvery; the cache still
// queries at most once per XIDCacheTTL.
func (m *Master) shareGPUXIDs(ctx context.Context) {
	cache := m.gpuXIDs()
	for _, r := range m.allRms {
		if agentRM, ok := r.(*agentrm.ResourceManager); ok {
			agentRM.SetGPUXIDs(cache.LastOK)
		}
	}
	if !m.config.Integrations.TaskResources.Enabled() {
		return
	}
	go refreshGPUXIDs(ctx, cache, gpuXIDRefreshEvery)
}

func refreshGPUXIDs(ctx context.Context, cache *gpuhealth.XIDCache, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		cache.Get(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
