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

// gpuMemoryFieldLabel names, in the query's result, which reading a series carries: a field
// name for the value, or the field name and "_at" for the sample's time.
const gpuMemoryFieldLabel = "det_gpu_memory_field"

const (
	gpuMemoryTimeout = 5 * time.Second
	// The largest reading accepted, in MiB: 16 TiB.
	gpuMemoryMaxMiB = 1 << 24
)

// gpuMemoryFields are the DCGM-Exporter gauges of GpuMemory, all in MiB.
var gpuMemoryFields = []struct{ name, metric string }{
	{"total", "DCGM_FI_DEV_FB_TOTAL"},
	{"used", "DCGM_FI_DEV_FB_USED"},
	{"free", "DCGM_FI_DEV_FB_FREE"},
	{"reserved", "DCGM_FI_DEV_FB_RESERVED"},
}

var (
	errGPUMemoryNotConfigured = status.Error(codes.FailedPrecondition,
		"GPU memory requires integrations.task_resources")
	errGPUMemoryQuery   = status.Error(codes.Unavailable, "the GPU memory query failed")
	errGPUMemoryInvalid = status.Error(codes.Internal,
		"the GPU memory query returned conflicting or invalid samples")
	errGPUMemoryNoSamples = status.Error(codes.Unavailable, "no GPU memory samples")
)

// gpuMemoryQuery evaluates every reading and its sample time once, at the query's one time.
func gpuMemoryQuery(cluster string) string {
	parts := make([]string, 0, 2*len(gpuMemoryFields))
	for _, f := range gpuMemoryFields {
		selector := f.metric + `{job="dcgm",det_cluster=` + promLabel(cluster) + `,gpu_uuid!=""}`
		tag := func(expr, value string) string {
			return "label_replace(" + expr + `, "` + gpuMemoryFieldLabel + `", "` + value + `", "", "")`
		}
		parts = append(parts, tag(selector, f.name), tag("timestamp("+selector+")", f.name+"_at"))
	}
	return strings.Join(parts, " or ")
}

// readGPUMemory returns each GPU's latest memory readings by GPU UUID, from one Prometheus query.
func readGPUMemory(
	ctx context.Context, conf config.TaskResourcesConfig, now time.Time,
) (map[string]*agentv1.GpuMemory, error) {
	if !conf.Enabled() {
		return nil, errGPUMemoryNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, gpuMemoryTimeout)
	defer cancel()
	at := now.Unix()
	series, err := queryTaskPrometheus(ctx, conf.PrometheusURL, gpuMemoryQuery(conf.DetCluster),
		taskResourceRange{Start: at, End: at, Step: 1})
	if err != nil {
		log.WithError(err).Debug("GPU memory query failed")
		return nil, errGPUMemoryQuery
	}
	return parseGPUMemory(series)
}

// parseGPUMemory joins each reading with its sample time. Two series for one GPU and reading, a
// reading without its time, or a value that is not a whole number of MiB in range is invalid.
func parseGPUMemory(series []prometheusTaskSeries) (map[string]*agentv1.GpuMemory, error) {
	type pair struct{ value, at *float64 }
	readings := map[string]map[string]*pair{}
	for _, s := range series {
		uuid, tag := s.Metric["gpu_uuid"], s.Metric[gpuMemoryFieldLabel]
		field, isTime := strings.CutSuffix(tag, "_at")
		if uuid == "" || !knownGPUMemoryField(field) || len(s.Samples) != 1 {
			return nil, errGPUMemoryInvalid
		}
		value, ok := s.Samples[0][1].(float64)
		if !ok {
			return nil, errGPUMemoryInvalid
		}
		if readings[uuid] == nil {
			readings[uuid] = map[string]*pair{}
		}
		p := readings[uuid][field]
		if p == nil {
			p = &pair{}
			readings[uuid][field] = p
		}
		slot := &p.value
		if isTime {
			slot = &p.at
		}
		if *slot != nil {
			return nil, errGPUMemoryInvalid
		}
		*slot = &value
	}

	out := make(map[string]*agentv1.GpuMemory, len(readings))
	for uuid, fields := range readings {
		memory := &agentv1.GpuMemory{}
		for name, p := range fields {
			if p.value == nil || p.at == nil {
				return nil, errGPUMemoryInvalid
			}
			mib := *p.value
			if mib < 0 || mib > gpuMemoryMaxMiB || mib != math.Trunc(mib) {
				return nil, errGPUMemoryInvalid
			}
			reading := &agentv1.GpuMemoryReading{
				Bytes:     int64(mib) << 20,
				SampledAt: timestamppb.New(time.UnixMilli(int64(math.Round(*p.at * 1000)))),
			}
			switch name {
			case "total":
				memory.Total = reading
			case "used":
				memory.Used = reading
			case "free":
				memory.Free = reading
			case "reserved":
				memory.Reserved = reading
			}
		}
		out[uuid] = memory
	}
	return out, nil
}

func knownGPUMemoryField(name string) bool {
	for _, f := range gpuMemoryFields {
		if f.name == name {
			return true
		}
	}
	return false
}

// applyGPUMemory gives every GPU of the agents' topologies its readings, all null for a GPU
// without samples. It fails when there are GPUs and none of them has a sample.
func applyGPUMemory(agents []*agentv1.Agent, memory map[string]*agentv1.GpuMemory) error {
	gpus, measured := 0, 0
	for _, agent := range agents {
		for _, gpu := range agent.GetGpuTopology().GetGpus() {
			gpus++
			if m, ok := memory[gpu.Uuid]; ok && gpu.Uuid != "" {
				gpu.Memory = m
				measured++
			} else {
				gpu.Memory = &agentv1.GpuMemory{}
			}
		}
	}
	if gpus > 0 && measured == 0 {
		return errGPUMemoryNoSamples
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
