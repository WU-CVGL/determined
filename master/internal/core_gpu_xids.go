package internal

import (
	"context"
	"math"
	"time"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/gpuhealth"
)

// gpuXIDs returns the master's one cache of the GPUs' recent critical XIDs. It queries the
// Prometheus of integrations.task_resources; without it, the status is NOT_CONFIGURED.
func (m *Master) gpuXIDs() *gpuhealth.XIDCache {
	m.xidCacheOnce.Do(func() {
		if m.xidCache == nil {
			conf := m.config.Integrations.TaskResources
			m.xidCache = gpuhealth.NewXIDCache(conf.DetCluster, gpuXIDQuery(conf))
		}
	})
	return m.xidCache
}

// gpuXIDQuery runs the XID range query with the client and checks of the task resource queries,
// or is nil without a Prometheus.
func gpuXIDQuery(conf config.TaskResourcesConfig) gpuhealth.RangeQuery {
	if !conf.Enabled() {
		return nil
	}
	return func(
		ctx context.Context, expr string, start, end time.Time, step time.Duration,
	) ([]gpuhealth.Series, error) {
		results, err := queryTaskPrometheus(ctx, conf.PrometheusURL, expr, taskResourceRange{
			Start: start.Unix(), End: end.Unix(), Step: int(step.Seconds()),
		})
		if err != nil {
			return nil, err
		}
		series := make([]gpuhealth.Series, 0, len(results))
		for _, r := range results {
			s := gpuhealth.Series{Labels: r.Metric}
			for _, p := range r.Samples {
				stamp, okStamp := p[0].(float64)
				value, okValue := p[1].(float64)
				if okStamp && okValue {
					s.Samples = append(s.Samples, gpuhealth.Sample{
						Time: time.UnixMilli(int64(math.Round(stamp * 1000))), Value: value,
					})
				}
			}
			series = append(series, s)
		}
		return series, nil
	}
}
