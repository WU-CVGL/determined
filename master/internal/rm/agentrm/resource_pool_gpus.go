package agentrm

import (
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
)

// gpuPolicy is a pool's GPU selection for one scheduling pass. The scheduler's simulation and the
// pass's live reservations use the same one, so they choose the same devices for the same state.
// Its zero value takes devices in map order.
type gpuPolicy struct {
	// packNUMA packs every task's GPUs by NUMA node: fitting_policy best.
	packNUMA bool
	// xids holds the UUIDs of GPUs with a recent critical XID.
	xids map[string]bool
}

// newGPUPolicy returns the pool's GPU selection for a pass. It reads the pool's config, which
// also built the pool's fitting method, so packing and the fitting policy never disagree.
func (rp *resourcePool) newGPUPolicy() gpuPolicy {
	return gpuPolicy{packNUMA: rp.config.Scheduler.FittingPolicy == best}
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
