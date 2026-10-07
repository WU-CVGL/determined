package agentrm

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/determined-ai/determined/master/internal/gpuhealth"
	"github.com/determined-ai/determined/master/internal/rm/rmevents"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// gpuXIDReader is where the pools of an agent RM read the GPUs' recent critical XIDs. The master
// sets it once (ResourceManager.SetGPUXIDs); until then, only NVML errors put a GPU in error.
type gpuXIDReader struct {
	read atomic.Pointer[func() *gpuhealth.XIDSnapshot]
}

func (r *gpuXIDReader) set(read func() *gpuhealth.XIDSnapshot) {
	r.read.Store(&read)
}

// recent returns the UUIDs of the GPUs with a critical XID in the last successful query whose last
// window is in the 24 hours a query at now would cover (gpuhealth.XIDRange): the XIDs that a failed
// query at now keeps. So the XIDs of an old result age as in the agent API. It never queries, so a
// scheduling pass never waits for Prometheus: a background refresh keeps the result current. It
// reads the snapshot without a lock, from every pool's scheduler: the cache must never change a
// snapshot after storing it.
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
	start, _ := gpuhealth.XIDRange(now)
	out := map[string]bool{}
	for uuid, xids := range snapshot.ByUUID {
		for _, x := range xids {
			if gpuhealth.IsCriticalXID(x.Code) && !x.LastObserved.Before(start) {
				out[uuid] = true
				break
			}
		}
	}
	return out
}

// gpuPolicy is a pool's GPU selection for one scheduling pass, with one aged XID result for the
// whole pass. The scheduler's simulation and the pass's live reservations use the same one, and no
// reservation reads the XIDs under the agent's lock. A ranked selection (NUMA packing, "soft" when
// it ranks) then chooses the same devices in both for the same state and the same placements in
// the same order; map order gives no such guarantee (see deepCopy). Fits use counts only, so a
// difference never changes which tasks fit. Its zero value takes devices in map order.
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
// prefer_gpu_topology "soft" ranks GPU sets only on one agent: a multi-agent fit takes whole idle
// agents, so there is no choice.
func (p gpuPolicy) selection(req *sproto.AllocateRequest, fits []*fittingState) deviceSelection {
	return deviceSelection{
		packNUMA:       p.packNUMA,
		preferTopology: req.FittingRequirements.GPUTopology == expconf.GPUTopologySoft && len(fits) == 1,
		xids:           p.xids,
	}
}

// gpuReservation is one reservation of an allocation, for its logs.
type gpuReservation struct {
	fit         *fittingState
	containerID cproto.ID
	resp        allocateFreeDevicesResponse
}

// logGPUChoices logs how each reservation of an allocation chose its devices, once the allocation
// is published: a failed selection at Error, the rule or the reason for map order at Debug. For a
// task with prefer_gpu_topology "soft" and 2 or more slots, it also publishes one line to the task
// log and logs it at Info.
func (rp *resourcePool) logGPUChoices(req *sproto.AllocateRequest, reservations []gpuReservation) {
	if req.FittingRequirements.GPUTopology == expconf.GPUTopologySoft && req.SlotsNeeded >= 2 &&
		len(reservations) > 0 {
		msg := gpuTopologyPreferenceLine(reservations)
		rp.syslog.WithField("allocation-id", req.AllocationID).Info(msg)
		rmevents.Publish(req.AllocationID, &sproto.ContainerLog{
			ContainerID: reservations[0].containerID,
			Timestamp:   time.Now().UTC(),
			AuxMessage:  &msg,
			Level:       ptrs.Ptr(model.LogLevelInfo),
			AgentID:     ptrs.Ptr(string(reservations[0].fit.Agent.id)),
		})
	}
	for _, r := range reservations {
		log := rp.syslog.WithField("allocation-id", req.AllocationID).WithField("agent-id", r.fit.Agent.id)
		switch {
		case r.resp.failure != "":
			log.Errorf("agent %s: GPU selection failed (%s); slots chosen in map order",
				r.fit.Agent.id, r.resp.failure)
		case r.resp.choice.rule != "":
			log.Debugf("agent %s: slots %s (%s)", r.fit.Agent.id, idList(r.resp.devices), r.resp.choice.rule)
		case r.resp.choice.mapOrder != "":
			log.Debugf("agent %s: slots %s (map order: %s)",
				r.fit.Agent.id, idList(r.resp.devices), r.resp.choice.mapOrder)
		}
	}
}

// gpuTopologyPreferenceLine is the task-log line of prefer_gpu_topology "soft".
func gpuTopologyPreferenceLine(reservations []gpuReservation) string {
	if len(reservations) > 1 {
		return "GPU topology preference has no effect: the task uses whole agents"
	}
	r := reservations[0]
	switch {
	case r.resp.failure != "":
		return fmt.Sprintf("GPU topology preference: agent %s not ranked (GPU selection failed); "+
			"slots chosen in map order", r.fit.Agent.id)
	case r.resp.choice.worstPair != "":
		return fmt.Sprintf("GPU topology preference: agent %s, slots %s; %s",
			r.fit.Agent.id, idList(r.resp.devices), r.resp.choice.worstPair)
	default:
		return fmt.Sprintf("GPU topology preference: agent %s not ranked (%s); "+
			"slots chosen as for tasks without it", r.fit.Agent.id, r.resp.choice.unranked)
	}
}
