package gpuhealth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

const (
	unknown = agentv1.GpuHealth_GPU_HEALTH_UNSPECIFIED
	ok      = agentv1.GpuHealth_GPU_HEALTH_OK
	narrow  = agentv1.GpuHealth_GPU_HEALTH_LINK_BELOW_MAX
	failed  = agentv1.GpuHealth_GPU_HEALTH_ERROR
)

func TestClassify(t *testing.T) {
	gpu := func(width, widthMax, gen, genMax int32, nvmlError string, excluded bool) *agentv1.GpuInfo {
		return &agentv1.GpuInfo{
			PcieLinkWidth: width, PcieLinkWidthMax: widthMax, PcieLinkGen: gen, PcieLinkGenMax: genMax,
			NvmlError: nvmlError, Excluded: excluded,
		}
	}
	withXIDs := func(g *agentv1.GpuInfo, codes ...int32) *agentv1.GpuInfo {
		for _, c := range codes {
			g.RecentXids = append(g.RecentXids, &agentv1.GpuXid{Xid: c})
		}
		return g
	}
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
		{"a critical XID", "", withXIDs(gpu(16, 16, 4, 4, "", false), 79), failed, ""},
		{"a critical XID with a narrow link", "", withXIDs(gpu(8, 16, 4, 4, "", false), 48), failed, ""},
		{"a critical XID on an excluded GPU", "", withXIDs(gpu(16, 16, 4, 4, "", true), 94), failed, ""},
		{
			"a critical XID with an unknown topology", "not reported since the master started",
			withXIDs(gpu(0, 0, 0, 0, "", false), 79), failed, "the join is by UUID",
		},
		{
			"application XIDs", "", withXIDs(gpu(16, 16, 4, 4, "", false), 13, 31, 43, 45), ok,
			"user code causes 13, 31, 43 and 45",
		},
		{"application XIDs, narrow", "", withXIDs(gpu(8, 16, 4, 4, "", false), 13, 31, 43, 45), narrow, ""},
		{"XID 0", "", withXIDs(gpu(16, 16, 4, 4, "", false), 0), ok, ""},
		{"an application and a critical XID", "", withXIDs(gpu(16, 16, 4, 4, "", false), 13, 79), failed, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			topo := &agentv1.GpuTopology{UnknownReason: c.reason, Gpus: []*agentv1.GpuInfo{c.gpu}}
			Classify(topo)
			require.Equal(t, c.want, topo.Gpus[0].Health, c.comment)
		})
	}

	// An excluded GPU never changes a slot's state.
	topo := &agentv1.GpuTopology{Gpus: []*agentv1.GpuInfo{
		{DeviceId: 0, PcieLinkWidth: 16, PcieLinkWidthMax: 16},
		{DeviceId: -1, Excluded: true, NvmlError: "DeviceGetHandleByUUID: ERROR_GPU_IS_LOST (15)"},
	}}
	Classify(topo)
	require.Equal(t, ok, topo.Gpus[0].Health)
	require.Equal(t, failed, topo.Gpus[1].Health)

	Classify(nil) // agents without GPUs
}

func TestApply(t *testing.T) {
	queriedAt := time.Date(2026, 10, 7, 12, 31, 0, 0, time.UTC)
	first := time.Date(2026, 10, 7, 10, 10, 0, 0, time.UTC)
	last := time.Date(2026, 10, 7, 12, 35, 0, 0, time.UTC)
	topology := func() *agentv1.GpuTopology {
		return &agentv1.GpuTopology{Gpus: []*agentv1.GpuInfo{
			{DeviceId: 0, Uuid: "GPU-0", PcieLinkWidth: 16, PcieLinkWidthMax: 16},
			{DeviceId: 1, Uuid: "GPU-1", PcieLinkWidth: 8, PcieLinkWidthMax: 16},
			{DeviceId: -1, Uuid: "GPU-x", PcieLinkWidth: 16, PcieLinkWidthMax: 16, Excluded: true},
		}}
	}
	xids := &XIDSnapshot{
		Status:    agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK,
		QueriedAt: queriedAt,
		ByUUID: map[string][]XID{
			"GPU-1": {
				{Code: 48, FirstObserved: first, LastObserved: first},
				{Code: 79, FirstObserved: first, LastObserved: last},
			},
			"GPU-x": {{Code: 79, FirstObserved: last, LastObserved: last}},
			"GPU-y": {{Code: 79, FirstObserved: last, LastObserved: last}}, // not this agent's
		},
	}

	topo := topology()
	Apply(topo, xids)
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK, topo.XidQueryStatus)
	require.Empty(t, topo.XidQueryError)
	require.Equal(t, queriedAt, topo.XidQueriedAt.AsTime())
	require.Equal(t, []agentv1.GpuHealth{ok, failed, failed},
		[]agentv1.GpuHealth{topo.Gpus[0].Health, topo.Gpus[1].Health, topo.Gpus[2].Health})
	require.Empty(t, topo.Gpus[0].RecentXids)
	require.Len(t, topo.Gpus[1].RecentXids, 2)
	require.Equal(t, int32(48), topo.Gpus[1].RecentXids[0].Xid)
	require.Equal(t, int32(79), topo.Gpus[1].RecentXids[1].Xid)
	require.Equal(t, first, topo.Gpus[1].RecentXids[1].FirstObserved.AsTime())
	require.Equal(t, last, topo.Gpus[1].RecentXids[1].LastObserved.AsTime())
	require.Equal(t, int32(79), topo.Gpus[2].RecentXids[0].Xid)

	// A failed query leaves the health to the agent's report.
	topo = topology()
	Apply(topo, &XIDSnapshot{
		Status: agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_FAILED, Error: "timeout", QueriedAt: queriedAt,
	})
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_FAILED, topo.XidQueryStatus)
	require.Equal(t, "timeout", topo.XidQueryError)
	require.Equal(t, []agentv1.GpuHealth{ok, narrow, ok},
		[]agentv1.GpuHealth{topo.Gpus[0].Health, topo.Gpus[1].Health, topo.Gpus[2].Health})

	topo = topology()
	Apply(topo, notConfigured)
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_NOT_CONFIGURED, topo.XidQueryStatus)
	require.Nil(t, topo.XidQueriedAt)
	require.Equal(t, narrow, topo.Gpus[1].Health)

	// Without a result, only the health is classified.
	topo = topology()
	Apply(topo, nil)
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_UNSPECIFIED, topo.XidQueryStatus)
	require.Equal(t, narrow, topo.Gpus[1].Health)

	Apply(nil, xids)
}
