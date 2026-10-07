package internal

import (
	"context"
	"time"

	"github.com/determined-ai/determined/master/internal/gpuhealth"
	"github.com/determined-ai/determined/master/internal/rm/agentrm"
)

// shareGPUXIDs gives the agent resource managers the GPUs' recent critical XIDs for GPU selection,
// which ranks a GPU with one last. Selection never queries: it reads the last successful result
// (XIDCache.LastOK). With a Prometheus (integrations.task_resources), a background loop keeps that
// result current until ctx ends, asking the cache every XIDCacheTTL; the cache still queries at
// most once per XIDCacheTTL.
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
	go refreshGPUXIDs(ctx, cache, gpuhealth.XIDCacheTTL)
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
