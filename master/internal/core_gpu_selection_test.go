package internal

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/gpuhealth"
)

// With a Prometheus, the master keeps the XIDs that GPU selection reads current in the background.
func TestRefreshGPUXIDsKeepsLastOKCurrent(t *testing.T) {
	prom := newFakeXIDPrometheus(t, xidMatrix)
	m := &Master{config: &config.Config{}}
	m.config.Integrations.TaskResources = config.TaskResourcesConfig{PrometheusURL: prom.URL, DetCluster: "lab-a"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.shareGPUXIDs(ctx)
	require.Eventually(t, func() bool { return m.gpuXIDs().LastOK() != nil }, 5*time.Second, 10*time.Millisecond)
	require.Contains(t, m.gpuXIDs().LastOK().ByUUID, "GPU-1")
	require.Len(t, prom.got(), 1, "one query per XIDCacheTTL at most")
}

// The refresh asks just after the cache's TTL, so every tick refreshes: the XIDs that selection
// reads are about 30 s old at most, plus a query.
func TestGPUXIDRefreshEveryExceedsTheCacheTTL(t *testing.T) {
	require.Greater(t, gpuXIDRefreshEvery, gpuhealth.XIDCacheTTL)
	require.LessOrEqual(t, gpuXIDRefreshEvery, gpuhealth.XIDCacheTTL+5*time.Second)
}

// Without a Prometheus, nothing queries and only NVML errors count.
func TestShareGPUXIDsWithoutPrometheus(t *testing.T) {
	m := &Master{config: &config.Config{}}
	m.shareGPUXIDs(context.Background())
	require.Nil(t, m.gpuXIDs().LastOK())
}

func TestRefreshGPUXIDsStopsWithItsContext(t *testing.T) {
	prom := newFakeXIDPrometheus(t, xidMatrix)
	query := gpuXIDQuery(config.TaskResourcesConfig{PrometheusURL: prom.URL, DetCluster: "lab-a"})
	cache := gpuhealth.NewXIDCache("lab-a", query)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		refreshGPUXIDs(ctx, cache, time.Millisecond)
		close(done)
	}()
	require.Eventually(t, func() bool { return cache.LastOK() != nil }, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh loop did not stop")
	}
}
