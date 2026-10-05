package agentrm

import (
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// gpuPairKey is an unordered pair of slots, with a < b.
type gpuPairKey struct {
	a, b device.ID
}

// gpuPair is what an agent reported for a pair of slots. A is the lower device ID.
type gpuPair struct {
	level   aproto.GPULinkLevel
	nvlinks int
	p2pAToB aproto.GPUP2PCaps
	p2pBToA aproto.GPUP2PCaps
}

// gpuTopology is the GPU topology an agent reported at its last start, keyed by device ID for its
// CUDA slots. It is immutable: a new report replaces the pointer (agentState.setGPUTopology).
// GPUs that the agent's exclude list left out are kept apart, for display only, keyed by UUID:
// they are never slots.
type gpuTopology struct {
	unknownReason string
	collectedAt   time.Time
	driverVersion string

	// gpus holds the reported info of the CUDA slots.
	gpus map[device.ID]aproto.GPUInfo
	// pairs holds the reported pairs of CUDA slots. A missing pair is unknown.
	pairs map[gpuPairKey]gpuPair

	// excluded holds the excluded GPUs, in report order.
	excluded []aproto.GPUInfo
	// excludedLinks holds the links with at least one excluded end, with UUIDA < UUIDB.
	excludedLinks []aproto.GPULink
}

// newGPUTopology builds the master's view of an AgentStarted report. It maps UUIDs to device IDs
// for CUDA devices only, drops GPUs and links that are neither slots nor excluded, normalizes a
// reversed pair (swapping its two directions with it), and keeps excluded GPUs and their links
// apart. A GPU marked excluded whose UUID is a slot is treated as a slot.
func newGPUTopology(
	wire *aproto.GPUTopology, devices []device.Device, version string, log *logrus.Entry,
) *gpuTopology {
	slotIDs := map[string]device.ID{}
	for _, d := range devices {
		if d.Type == device.CUDA {
			slotIDs[d.UUID] = d.ID
		}
	}
	g := &gpuTopology{
		gpus:  map[device.ID]aproto.GPUInfo{},
		pairs: map[gpuPairKey]gpuPair{},
	}
	if wire == nil {
		if len(slotIDs) > 0 {
			g.unknownReason = fmt.Sprintf("agent %s does not report GPU topology", version)
		}
		return g
	}
	g.unknownReason = wire.UnknownReason
	g.collectedAt = wire.CollectedAt
	g.driverVersion = wire.DriverVersion

	excluded := map[string]bool{}
	for _, info := range wire.GPUs {
		if id, ok := slotIDs[info.UUID]; ok {
			if info.Excluded {
				log.Warnf("GPU %s is reported as excluded but is slot %d; treating it as a slot",
					info.UUID, id)
				info.Excluded = false
			}
			if _, dup := g.gpus[id]; !dup {
				g.gpus[id] = info
			}
			continue
		}
		if !info.Excluded {
			log.Debugf("ignoring GPU %s in the topology report: not a CUDA slot", info.UUID)
			continue
		}
		if !excluded[info.UUID] {
			excluded[info.UUID] = true
			g.excluded = append(g.excluded, info)
		}
	}

	seenExcludedLinks := map[[2]string]bool{}
	for _, l := range wire.Links {
		idA, slotA := slotIDs[l.UUIDA]
		idB, slotB := slotIDs[l.UUIDB]
		switch {
		case l.UUIDA == l.UUIDB:
			continue
		case slotA && slotB:
			key := gpuPairKey{a: idA, b: idB}
			pair := gpuPair{level: l.Level, nvlinks: l.NVLinks, p2pAToB: l.P2PAToB, p2pBToA: l.P2PBToA}
			if idB < idA {
				key = gpuPairKey{a: idB, b: idA}
				pair.p2pAToB, pair.p2pBToA = l.P2PBToA, l.P2PAToB
			}
			if _, dup := g.pairs[key]; !dup {
				g.pairs[key] = pair
			}
		case (slotA || excluded[l.UUIDA]) && (slotB || excluded[l.UUIDB]):
			if l.UUIDB < l.UUIDA {
				l.UUIDA, l.UUIDB = l.UUIDB, l.UUIDA
				l.P2PAToB, l.P2PBToA = l.P2PBToA, l.P2PAToB
			}
			key := [2]string{l.UUIDA, l.UUIDB}
			if !seenExcludedLinks[key] {
				seenExcludedLinks[key] = true
				g.excludedLinks = append(g.excludedLinks, l)
			}
		default:
			log.Debugf("ignoring GPU link %s-%s in the topology report: not slots", l.UUIDA, l.UUIDB)
		}
	}
	return g
}
