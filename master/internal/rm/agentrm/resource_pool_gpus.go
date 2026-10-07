package agentrm

import (
	"sync/atomic"
	"time"

	"github.com/determined-ai/determined/master/internal/gpuhealth"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
)

// gpuXIDReader is where the pools of an agent RM read the GPUs' recent critical XIDs. The master
// sets it once (ResourceManager.SetGPUXIDs); until then, only NVML errors put a GPU in error.
type gpuXIDReader struct {
	read atomic.Pointer[func() *gpuhealth.XIDSnapshot]
}

func (r *gpuXIDReader) set(read func() *gpuhealth.XIDSnapshot) {
	r.read.Store(&read)
}

// recent returns the UUIDs of the GPUs with a critical XID last observed less than XIDWindow
// before now, from the last successful query. It never queries, so a scheduling pass never waits
// for Prometheus: a background refresh keeps the result current. An XID of an old result that has
// left the window no longer counts.
func (r *gpuXIDReader) recent(now time.Time) map[string]bool {
	if r == nil {
		return nil
	}
	read := r.read.Load()
	if read == nil {
		return nil
	}
	snapshot := (*read)()
	if snapshot == nil {
		return nil
	}
	out := map[string]bool{}
	for uuid, xids := range snapshot.ByUUID {
		for _, x := range xids {
			if gpuhealth.IsCriticalXID(x.Code) && now.Sub(x.LastObserved) < gpuhealth.XIDWindow {
				out[uuid] = true
				break
			}
		}
	}
	return out
}

// gpuPolicy is a pool's GPU selection for one scheduling pass. The scheduler's simulation and the
// pass's live reservations use the same one, so they choose the same devices for the same state.
// Its zero value takes devices in map order.
type gpuPolicy struct {
	// packNUMA packs every task's GPUs by NUMA node: fitting_policy best, numa_packing not false.
	packNUMA bool
	// xids holds the UUIDs of GPUs with a recent critical XID.
	xids map[string]bool
}

// newGPUPolicy returns the pool's GPU selection for a pass. It reads the pool's config, which
// also built the pool's fitting method, so packing and the fitting policy never disagree, and one
// XID result for the whole pass.
func (rp *resourcePool) newGPUPolicy() gpuPolicy {
	return gpuPolicy{
		packNUMA: rp.config.Scheduler.PacksGPUsByNUMA(),
		xids:     rp.gpuXIDs.recent(time.Now()),
	}
}

// selection returns how a request's reservation on one of its fits chooses its devices.
func (p gpuPolicy) selection(_ *sproto.AllocateRequest, _ []*fittingState) deviceSelection {
	return deviceSelection{packNUMA: p.packNUMA, xids: p.xids}
}

// gpuReservation is one reservation of an allocation, for its logs.
type gpuReservation struct {
	fit         *fittingState
	containerID cproto.ID
	resp        allocateFreeDevicesResponse
}

// logGPUChoices logs how each reservation of an allocation chose its devices, once the allocation
// is published: a selection that fell back to map order at Error, the rule at Debug.
func (rp *resourcePool) logGPUChoices(req *sproto.AllocateRequest, reservations []gpuReservation) {
	for _, r := range reservations {
		log := rp.syslog.WithField("allocation-id", req.AllocationID).WithField("agent-id", r.fit.Agent.id)
		switch {
		case r.resp.failure != "":
			log.Errorf("agent %s: GPU selection failed (%s); slots chosen in map order",
				r.fit.Agent.id, r.resp.failure)
		case r.resp.choice.rule != "":
			log.Debugf("agent %s: slots %s (%s)", r.fit.Agent.id, idList(r.resp.devices), r.resp.choice.rule)
		}
	}
}
