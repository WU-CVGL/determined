package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
	"github.com/determined-ai/determined/proto/pkg/containerv1"
	"github.com/determined-ai/determined/proto/pkg/devicev1"
)

func TestSummarizeSlots_EmptySlots(t *testing.T) {
	slots := make(map[string]*agentv1.Slot)
	stats := model.SummarizeSlots(slots)

	assert.Empty(t, len(stats.TypeStats))
	assert.Empty(t, len(stats.BrandStats))
}

func TestSummarizeSlots_VariousStates(t *testing.T) {
	slots := map[string]*agentv1.Slot{
		"slot1": {
			Device: &devicev1.Device{
				Type:  devicev1.Type_TYPE_CUDA,
				Brand: "Nvidia",
			},
			Enabled:   true,
			Draining:  false,
			Container: &containerv1.Container{State: containerv1.State_STATE_RUNNING},
		},
		"slot2": {
			Device: &devicev1.Device{
				Type:  devicev1.Type_TYPE_CUDA,
				Brand: "Nvidia",
			},
			Enabled:  false,
			Draining: false,
		},
		"slot3": {
			Device: &devicev1.Device{
				Type:  devicev1.Type_TYPE_CPU,
				Brand: "Intel",
			},
			Enabled:  true,
			Draining: true,
		},
	}

	stats := model.SummarizeSlots(slots)

	assert.Equal(t, 2, int(stats.TypeStats[devicev1.Type_TYPE_CUDA.String()].Total))
	assert.Equal(t, 1, int(stats.TypeStats[devicev1.Type_TYPE_CPU.String()].Total))
	assert.Equal(t, 1, int(stats.TypeStats[devicev1.Type_TYPE_CUDA.String()].Disabled))
	assert.Equal(t, 1, int(stats.TypeStats[devicev1.Type_TYPE_CPU.String()].Draining))
	assert.Equal(t, 1, int(stats.TypeStats[devicev1.Type_TYPE_CUDA.String()].
		States[containerv1.State_STATE_RUNNING.String()]))

	assert.Equal(t, 2, int(stats.BrandStats["Nvidia"].Total))
	assert.Equal(t, 1, int(stats.BrandStats["Intel"].Total))
	assert.Equal(t, 1, int(stats.BrandStats["Nvidia"].Disabled))
	assert.Equal(t, 1, int(stats.BrandStats["Intel"].Draining))
}

func TestClassifyGPUHealth(t *testing.T) {
	gpu := func(width, widthMax, gen, genMax int32, nvmlError string, excluded bool) *agentv1.GpuInfo {
		return &agentv1.GpuInfo{
			PcieLinkWidth: width, PcieLinkWidthMax: widthMax, PcieLinkGen: gen, PcieLinkGenMax: genMax,
			NvmlError: nvmlError, Excluded: excluded,
		}
	}
	const (
		unknown = agentv1.GpuHealth_GPU_HEALTH_UNSPECIFIED
		ok      = agentv1.GpuHealth_GPU_HEALTH_OK
		narrow  = agentv1.GpuHealth_GPU_HEALTH_LINK_BELOW_MAX
		failed  = agentv1.GpuHealth_GPU_HEALTH_ERROR
	)
	cases := []struct {
		name    string
		reason  string
		gpu     *agentv1.GpuInfo
		want    agentv1.GpuHealth
		comment string
	}{
		{"x16 of x16", "", gpu(16, 16, 4, 4, "", false), ok, ""},
		{"x8 of x16", "", gpu(8, 16, 4, 4, "", false), narrow, ""},
		{
			"an NVML error wins", "", gpu(16, 16, 4, 4, "GetMaxPcieLinkGeneration: ERROR_GPU_IS_LOST (15)", false),
			failed, "",
		},
		{"an NVML error with a narrow link", "", gpu(8, 16, 4, 4, "GetPciInfo: ERROR_UNKNOWN (999)", false), failed, ""},
		{"Gen1 of Gen4 at x16", "", gpu(16, 16, 1, 4, "", false), ok, "the generation never changes the state"},
		{"Gen1 of Gen4 at x8", "", gpu(8, 16, 1, 4, "", false), narrow, "the generation never changes the state"},
		{"current width unknown", "", gpu(0, 16, 4, 4, "", false), unknown, ""},
		{"max width unknown", "", gpu(16, 0, 4, 4, "", false), unknown, ""},
		{"current above max", "", gpu(16, 8, 4, 4, "", false), unknown, ""},
		{"topology unknown", "NVML init: ERROR_LIBRARY_NOT_FOUND (12)", gpu(0, 0, 0, 0, "", false), unknown, ""},
		{
			"not reported since the master started", "not reported since the master started",
			gpu(16, 16, 4, 4, "", false), unknown, "",
		},
		{"an excluded GPU at x16", "", gpu(16, 16, 4, 4, "", true), ok, ""},
		{"an excluded GPU in error", "", gpu(16, 16, 4, 4, "GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)", true), failed, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			topo := &agentv1.GpuTopology{UnknownReason: c.reason, Gpus: []*agentv1.GpuInfo{c.gpu}}
			classifyGPUHealth(topo)
			require.Equal(t, c.want, topo.Gpus[0].Health, c.comment)
		})
	}

	// An excluded GPU never changes a slot's state.
	topo := &agentv1.GpuTopology{Gpus: []*agentv1.GpuInfo{
		{DeviceId: 0, PcieLinkWidth: 16, PcieLinkWidthMax: 16},
		{DeviceId: -1, Excluded: true, NvmlError: "DeviceGetHandleByUUID: ERROR_GPU_IS_LOST (15)"},
	}}
	classifyGPUHealth(topo)
	require.Equal(t, ok, topo.Gpus[0].Health)
	require.Equal(t, failed, topo.Gpus[1].Health)

	classifyGPUHealth(nil) // agents without GPUs
}
