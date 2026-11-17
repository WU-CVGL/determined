package internal

import (
	"context"
	"math"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

// gpuReadingLabel names, in the query's result, which reading a series carries: a field key
// for the value, or the key and "_at" for the sample's time.
const gpuReadingLabel = "det_gpu_reading"

const (
	gpuReadingTimeout = 5 * time.Second
	// The largest memory reading accepted, in MiB: 16 TiB.
	gpuMemoryMaxMiB = 1 << 24
	// The largest power reading accepted, in W, as for the task resource power.
	gpuPowerMaxWatts = 10000
)

const (
	gpuMemoryGroup = "memory"
	gpuPowerGroup  = "power"
)

// gpuReadingField is one DCGM-Exporter gauge: memory in MiB, power in W.
type gpuReadingField struct{ group, name, metric string }

func (f gpuReadingField) key() string { return f.group + "_" + f.name }

var gpuReadingFields = []gpuReadingField{
	{gpuMemoryGroup, "total", "DCGM_FI_DEV_FB_TOTAL"},
	{gpuMemoryGroup, "used", "DCGM_FI_DEV_FB_USED"},
	{gpuMemoryGroup, "free", "DCGM_FI_DEV_FB_FREE"},
	{gpuMemoryGroup, "reserved", "DCGM_FI_DEV_FB_RESERVED"},
	{gpuPowerGroup, "draw", "DCGM_FI_DEV_POWER_USAGE"},
	{gpuPowerGroup, "limit", "DCGM_FI_DEV_POWER_MGMT_LIMIT"},
	{gpuPowerGroup, "default_limit", "DCGM_FI_DEV_POWER_MGMT_LIMIT_DEF"},
	{gpuPowerGroup, "enforced_limit", "DCGM_FI_DEV_ENFORCED_POWER_LIMIT"},
}

// gpuReadingGroups says which readings a request asks for.
type gpuReadingGroups struct{ memory, power bool }

func (g gpuReadingGroups) any() bool { return g.memory || g.power }

func (g gpuReadingGroups) has(group string) bool {
	return (group == gpuMemoryGroup && g.memory) || (group == gpuPowerGroup && g.power)
}

var (
	errGPUReadingsNotConfigured = status.Error(codes.FailedPrecondition,
		"GPU memory and power require integrations.task_resources")
	errGPUReadingsQuery   = status.Error(codes.Unavailable, "the GPU reading query failed")
	errGPUReadingsInvalid = status.Error(codes.Internal,
		"the GPU reading query returned conflicting or invalid samples")
	errGPUMemoryNoSamples = status.Error(codes.Unavailable, "no GPU memory samples")
	errGPUPowerNoSamples  = status.Error(codes.Unavailable, "no GPU power samples")
)

// gpuReadingsQuery evaluates every requested reading and its sample time once, at the query's
// one time.
func gpuReadingsQuery(cluster string, groups gpuReadingGroups) string {
	var parts []string
	for _, f := range gpuReadingFields {
		if !groups.has(f.group) {
			continue
		}
		selector := f.metric + `{job="dcgm",det_cluster=` + promLabel(cluster) + `,gpu_uuid!=""}`
		tag := func(expr, value string) string {
			return "label_replace(" + expr + `, "` + gpuReadingLabel + `", "` + value + `", "", "")`
		}
		parts = append(parts, tag(selector, f.key()), tag("timestamp("+selector+")", f.key()+"_at"))
	}
	return strings.Join(parts, " or ")
}

// gpuReadings are one GPU's requested readings; a group without any sample is nil.
type gpuReadings struct {
	memory *agentv1.GpuMemory
	power  *agentv1.GpuPower
}

// readGPUReadings returns each GPU's latest requested readings by GPU UUID, from one Prometheus
// query.
func readGPUReadings(
	ctx context.Context, conf config.TaskResourcesConfig, now time.Time, groups gpuReadingGroups,
) (map[string]gpuReadings, error) {
	if !conf.Enabled() {
		return nil, errGPUReadingsNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, gpuReadingTimeout)
	defer cancel()
	at := now.Unix()
	series, err := queryTaskPrometheus(ctx, conf.PrometheusURL,
		gpuReadingsQuery(conf.DetCluster, groups), taskResourceRange{Start: at, End: at, Step: 1})
	if err != nil {
		log.WithError(err).Debug("GPU reading query failed")
		return nil, errGPUReadingsQuery
	}
	return parseGPUReadings(series, groups)
}

// gpuSample is a reading's value and its sample time.
type gpuSample struct {
	value float64
	at    *timestamppb.Timestamp
}

// pairGPUSamples joins each reading with its sample time, by GPU UUID and field key. Two series
// for one GPU and reading, a reading without its time, a field that was not requested, or a
// value that is not a number is invalid.
func pairGPUSamples(
	series []prometheusTaskSeries, groups gpuReadingGroups,
) (map[string]map[string]gpuSample, error) {
	requested := map[string]bool{}
	for _, f := range gpuReadingFields {
		requested[f.key()] = groups.has(f.group)
	}
	type pair struct{ value, at *float64 }
	pairs := map[string]map[string]*pair{}
	for _, s := range series {
		uuid, tag := s.Metric["gpu_uuid"], s.Metric[gpuReadingLabel]
		key, isTime := strings.CutSuffix(tag, "_at")
		if uuid == "" || !requested[key] || len(s.Samples) != 1 {
			return nil, errGPUReadingsInvalid
		}
		value, ok := s.Samples[0][1].(float64)
		if !ok {
			return nil, errGPUReadingsInvalid
		}
		if pairs[uuid] == nil {
			pairs[uuid] = map[string]*pair{}
		}
		p := pairs[uuid][key]
		if p == nil {
			p = &pair{}
			pairs[uuid][key] = p
		}
		slot := &p.value
		if isTime {
			slot = &p.at
		}
		if *slot != nil {
			return nil, errGPUReadingsInvalid
		}
		*slot = &value
	}

	out := make(map[string]map[string]gpuSample, len(pairs))
	for uuid, keys := range pairs {
		out[uuid] = map[string]gpuSample{}
		for key, p := range keys {
			if p.value == nil || p.at == nil {
				return nil, errGPUReadingsInvalid
			}
			out[uuid][key] = gpuSample{
				value: *p.value,
				at:    timestamppb.New(time.UnixMilli(int64(math.Round(*p.at * 1000)))),
			}
		}
	}
	return out, nil
}

// parseGPUReadings converts each GPU's samples: memory from whole MiB to bytes, power in W.
func parseGPUReadings(
	series []prometheusTaskSeries, groups gpuReadingGroups,
) (map[string]gpuReadings, error) {
	samples, err := pairGPUSamples(series, groups)
	if err != nil {
		return nil, err
	}
	out := make(map[string]gpuReadings, len(samples))
	for uuid, byKey := range samples {
		var r gpuReadings
		for _, f := range gpuReadingFields {
			s, ok := byKey[f.key()]
			if !ok {
				continue
			}
			switch f.group {
			case gpuMemoryGroup:
				if s.value < 0 || s.value > gpuMemoryMaxMiB || s.value != math.Trunc(s.value) {
					return nil, errGPUReadingsInvalid
				}
				if r.memory == nil {
					r.memory = &agentv1.GpuMemory{}
				}
				setGPUMemory(r.memory, f.name, &agentv1.GpuMemoryReading{
					Bytes: int64(s.value) << 20, SampledAt: s.at,
				})
			case gpuPowerGroup:
				if s.value < 0 || s.value > gpuPowerMaxWatts {
					return nil, errGPUReadingsInvalid
				}
				if r.power == nil {
					r.power = &agentv1.GpuPower{}
				}
				setGPUPower(r.power, f.name, &agentv1.GpuPowerReading{Watts: s.value, SampledAt: s.at})
			}
		}
		out[uuid] = r
	}
	return out, nil
}

func setGPUMemory(m *agentv1.GpuMemory, name string, r *agentv1.GpuMemoryReading) {
	switch name {
	case "total":
		m.Total = r
	case "used":
		m.Used = r
	case "free":
		m.Free = r
	case "reserved":
		m.Reserved = r
	}
}

func setGPUPower(p *agentv1.GpuPower, name string, r *agentv1.GpuPowerReading) {
	switch name {
	case "draw":
		p.Draw = r
	case "limit":
		p.Limit = r
	case "default_limit":
		p.DefaultLimit = r
	case "enforced_limit":
		p.EnforcedLimit = r
	}
}

// applyGPUReadings gives every GPU of the agents' topologies its requested readings, all null for
// a GPU without samples. It fails when there are GPUs and none of them has a sample of a
// requested group.
func applyGPUReadings(
	agents []*agentv1.Agent, readings map[string]gpuReadings, groups gpuReadingGroups,
) error {
	gpus, withMemory, withPower := 0, 0, 0
	for _, agent := range agents {
		for _, gpu := range agent.GetGpuTopology().GetGpus() {
			gpus++
			r := gpuReadings{}
			if gpu.Uuid != "" {
				r = readings[gpu.Uuid]
			}
			if groups.memory {
				gpu.Memory = r.memory
				if gpu.Memory == nil {
					gpu.Memory = &agentv1.GpuMemory{}
				} else {
					withMemory++
				}
			}
			if groups.power {
				gpu.Power = r.power
				if gpu.Power == nil {
					gpu.Power = &agentv1.GpuPower{}
				} else {
					withPower++
				}
			}
		}
	}
	switch {
	case gpus > 0 && groups.memory && withMemory == 0:
		return errGPUMemoryNoSamples
	case gpus > 0 && groups.power && withPower == 0:
		return errGPUPowerNoSamples
	}
	return nil
}

// countTopologyGPUs counts the GPUs of the agents' topologies.
func countTopologyGPUs(agents []*agentv1.Agent) int {
	n := 0
	for _, agent := range agents {
		n += len(agent.GetGpuTopology().GetGpus())
	}
	return n
}
