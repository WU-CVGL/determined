package gpuhealth

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

// Apply sets the XID fields of an agent's GPU topology from xids when it is not nil, joining by GPU
// UUID, so slots, excluded GPUs and GPUs of an unknown topology all get theirs, and then classifies
// the health of every GPU (Classify). It does nothing for a nil topology.
func Apply(topo *agentv1.GpuTopology, xids *XIDSnapshot) {
	if topo == nil {
		return
	}
	if xids != nil {
		topo.XidQueryStatus = xids.Status
		topo.XidQueryError = xids.Error
		topo.XidQueriedAt = nil
		if !xids.QueriedAt.IsZero() {
			topo.XidQueriedAt = timestamppb.New(xids.QueriedAt)
		}
		for _, g := range topo.Gpus {
			g.RecentXids = nil
			for _, x := range xids.ByUUID[g.Uuid] {
				g.RecentXids = append(g.RecentXids, &agentv1.GpuXid{
					Xid:           int32(x.Code),
					FirstObserved: timestamppb.New(x.FirstObserved),
					LastObserved:  timestamppb.New(x.LastObserved),
				})
			}
		}
	}
	Classify(topo)
}

// Classify sets the health of every GPU of an agent. The first matching row wins:
//   - ERROR: GPU-side evidence: an NVML health call failed at the agent's last start, or the GPU
//     has a recent critical XID (IsCriticalXID);
//   - LINK_BELOW_MAX: at agent start, the current and maximum link widths were both known and
//     current < max;
//   - OK: the topology is known, and at agent start both widths were known and equal;
//   - UNKNOWN (unspecified): anything else.
//
// The link generation never changes the state, and an excluded GPU gets its own state by the same
// rules. A task that fails is no evidence: user code fails the same way. It is the only place the
// state is computed, so the CLI and the WebUI never disagree, and GPU selection is to read the same
// state (PR B).
func Classify(topo *agentv1.GpuTopology) {
	if topo == nil {
		return
	}
	known := topo.UnknownReason == ""
	for _, g := range topo.Gpus {
		widthsKnown := g.PcieLinkWidth > 0 && g.PcieLinkWidthMax > 0
		switch {
		case g.NvmlError != "" || hasCriticalXID(g):
			g.Health = agentv1.GpuHealth_GPU_HEALTH_ERROR
		case widthsKnown && g.PcieLinkWidth < g.PcieLinkWidthMax:
			g.Health = agentv1.GpuHealth_GPU_HEALTH_LINK_BELOW_MAX
		case known && widthsKnown && g.PcieLinkWidth == g.PcieLinkWidthMax:
			g.Health = agentv1.GpuHealth_GPU_HEALTH_OK
		default:
			g.Health = agentv1.GpuHealth_GPU_HEALTH_UNSPECIFIED
		}
	}
}

func hasCriticalXID(g *agentv1.GpuInfo) bool {
	for _, x := range g.RecentXids {
		if IsCriticalXID(int(x.Xid)) {
			return true
		}
	}
	return false
}
