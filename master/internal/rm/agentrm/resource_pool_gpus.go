package agentrm

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/determined-ai/determined/master/internal/gpuhealth"
	"github.com/determined-ai/determined/master/internal/rm/rmevents"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
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
// it ranks, "strong") then chooses the same devices in both for the same placements in the same
// order while every earlier placement on the agent in the pass ranked too; map order gives no such
// guarantee (see deepCopy). Fits use counts only, so a difference never changes which tasks fit,
// except for "strong", and never changes the agent a task gets, except for "strong" and for "soft"
// under NUMA packing (preferOneNUMANode). Its zero value takes a plain task's devices in map order.
type gpuPolicy struct {
	// packNUMA packs every task's GPUs by NUMA node: fitting_policy best, numa_packing not false.
	// It is also the gate of the agent choice of "soft" (preferOneNUMANode), which every findFits
	// of the pass reads.
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
// agents, so there is no choice. "strong" with 2 or more slots fits only on one agent.
func (p gpuPolicy) selection(req *sproto.AllocateRequest, fits []*fittingState) deviceSelection {
	return deviceSelection{
		packNUMA:       p.packNUMA,
		preferTopology: req.FittingRequirements.GPUTopology == expconf.GPUTopologySoft && len(fits) == 1,
		strong:         strongTopology(req) && len(fits) == 1,
		xids:           p.xids,
	}
}

// strongTopology reports whether a request takes the GPUs of one NUMA node: prefer_gpu_topology
// "strong" with 2 or more slots. With fewer, "strong" is as no preference.
func strongTopology(req *sproto.AllocateRequest) bool {
	return req.FittingRequirements.GPUTopology == expconf.GPUTopologyStrong && req.SlotsNeeded >= 2
}

// preferOneNUMANode reports whether a request's single-agent fit puts the agents where one NUMA
// node has its slots free (holdsOnOneNUMANode) before the others: prefer_gpu_topology "soft" with 2
// or more slots, in a pool that packs GPUs by NUMA node (packNUMA). An agent with an unknown
// topology holds none, so it is with the agents that would split the task; the fitting score
// decides within each group. It needs packing: only then do the scheduler's copies and the live
// agents keep the same free GPUs on each NUMA node, which the order reads (see deepCopy).
func preferOneNUMANode(req *sproto.AllocateRequest, packNUMA bool) bool {
	return packNUMA && req.FittingRequirements.GPUTopology == expconf.GPUTopologySoft && req.SlotsNeeded >= 2
}

// gpuReservation is one reservation of an allocation, for its logs.
type gpuReservation struct {
	fit         *fittingState
	containerID cproto.ID
	resp        allocateFreeDevicesResponse
}

// logGPUChoices logs how each reservation of an allocation chose its devices, once the allocation
// is published: a failed selection at Error, the rule or the reason for map order at Debug. For a
// task with prefer_gpu_topology "soft" or "strong" and 2 or more slots, it also publishes one line
// to the task log and logs it at Info.
func (rp *resourcePool) logGPUChoices(req *sproto.AllocateRequest, reservations []gpuReservation) {
	pref := req.FittingRequirements.GPUTopology
	if (pref == expconf.GPUTopologySoft || pref == expconf.GPUTopologyStrong) && req.SlotsNeeded >= 2 &&
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

// gpuTopologyPreferenceLine is the task-log line of prefer_gpu_topology "soft" and "strong". A
// "strong" reservation always has a worst pair: it never falls back.
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

// strongNotice is what the pool told a pending task with prefer_gpu_topology "strong".
type strongNotice int

const (
	strongWaiting strongNotice = iota + 1
	strongFailed
)

// checkStrongRequests runs after the reservations of a scheduling pass, on the agent states as the
// pass left them (rp.agentStatesCache, refreshed for every agent the pass reserved on or failed
// on). For each pending request with prefer_gpu_topology "strong":
//   - if it fits and the pass allocated or failed a reservation, it asks for one more pass: the
//     pass's plan and its reservations can differ (deepCopy), and an agent can change under a
//     failed reservation. A pass that does neither, also one whose only failures are persistence
//     failures, does not ask again, so passes converge;
//   - if no agent of the pool can ever hold it (strongCannotFit), the request fails, once, with an
//     InvalidResourcesRequestError whose cause is that type too, so a trial ends without restarts;
//   - otherwise, if it does not fit, the task log says once that it waits.
//
// It returns whether to run another pass. It keeps one notice per task and drops the tasks that
// left the task list.
func (rp *resourcePool) checkStrongRequests(reserved bool) bool {
	if rp.strongNotices == nil {
		rp.strongNotices = map[model.AllocationID]strongNotice{}
	}
	for id := range rp.strongNotices {
		if _, ok := rp.taskList.TaskByID(id); !ok {
			delete(rp.strongNotices, id)
		}
	}
	again := false
	for it := rp.taskList.Iterator(); it.Next(); {
		req := it.Value()
		if !strongTopology(req) || rp.taskList.IsScheduled(req.AllocationID) {
			continue
		}
		fits := findFits(req, rp.agentStatesCache, rp.fittingMethod, rp.config.Scheduler.AllowHeterogeneousFits,
			rp.gpuPolicy.packNUMA)
		if len(fits) > 0 {
			again = again || reserved
			continue
		}
		log := rp.syslog.WithField("allocation-id", req.AllocationID)
		notice := rp.strongNotices[req.AllocationID]
		if cause := strongCannotFit(rp.config.PoolName, rp.agentStatesCache, req.SlotsNeeded); cause != nil {
			if notice != strongFailed {
				rp.strongNotices[req.AllocationID] = strongFailed
				log.Warnf("GPU topology preference strong: %s", cause)
				rmevents.Publish(req.AllocationID, &sproto.InvalidResourcesRequestError{
					Cause: sproto.InvalidResourcesRequestError{Cause: cause},
				})
			}
			continue
		}
		if notice == 0 {
			rp.strongNotices[req.AllocationID] = strongWaiting
			msg := fmt.Sprintf("GPU topology preference strong: waiting until one NUMA node of an agent "+
				"in pool %s has %d free GPUs", rp.config.PoolName, req.SlotsNeeded)
			log.Info(msg)
			rmevents.Publish(req.AllocationID, &sproto.ContainerLog{
				Timestamp:  time.Now().UTC(),
				AuxMessage: &msg,
				Level:      ptrs.Ptr(model.LogLevelInfo),
			})
		}
	}
	return again
}

// strongCannotFit says why no agent of the pool can ever hold n slots on one NUMA node, or returns
// nil while one might: the pool has no agent, or one has not reported its topology since the
// master started. Every slot counts, also disabled and draining ones.
func strongCannotFit(pool string, agents map[aproto.ID]*agentState, n int) error {
	if len(agents) == 0 {
		return nil
	}
	known := false
	for _, a := range agents {
		if a.gpuTopology == nil {
			return nil
		}
		for _, slots := range a.slotsByNUMA() {
			known = true
			if slots >= n {
				return nil
			}
		}
	}
	if !known {
		return errStrongNoNUMANodes(pool)
	}
	return errStrongNoNUMANodeHolds(pool, n)
}

// errStrongNoNUMANodes and errStrongNoNUMANodeHolds are why a task with prefer_gpu_topology
// "strong" is refused or fails.
func errStrongNoNUMANodes(pool string) error {
	return fmt.Errorf("no agent in pool %s reports NUMA nodes; use soft", pool)
}

func errStrongNoNUMANodeHolds(pool string, n int) error {
	return fmt.Errorf("no NUMA node in pool %s has %d slots; use soft", pool, n)
}
