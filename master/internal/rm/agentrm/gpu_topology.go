package agentrm

import (
	"fmt"
	"sort"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/determined-ai/determined/master/internal/gpuhealth"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/protoutils"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

// reasonNotReportedSinceMasterStart is the unknown reason of an agent restored from its snapshot
// whose AgentStarted has not arrived yet.
const reasonNotReportedSinceMasterStart = "not reported since the master started"

// excludedDeviceID is the device id of an excluded GPU, which is not a slot.
const excludedDeviceID = -1

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
	if wire.CollectedAt != nil {
		g.collectedAt = *wire.CollectedAt
	}
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

// gpuTopologyProto assembles the agent's GPU topology for the API: one entry per CUDA slot, with
// device_id and uuid from the slots, so the shape is the same when the topology is unknown; then
// the excluded GPUs, also when the topology is unknown. It is nil only for agents with neither
// CUDA slots nor excluded GPUs. Health is left to gpuhealth.Apply.
func (a *agentState) gpuTopologyProto() *agentv1.GpuTopology {
	var slots []device.Device
	slotOf := map[string]device.ID{}
	for _, s := range a.slotStates {
		if s.device.Type == device.CUDA {
			slots = append(slots, s.device)
			slotOf[s.device.UUID] = s.device.ID
		}
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })

	g := a.gpuTopology
	var excluded []aproto.GPUInfo
	if g != nil {
		excluded = append(excluded, g.excluded...)
	}
	if len(slots) == 0 && len(excluded) == 0 {
		return nil
	}

	out := &agentv1.GpuTopology{Gpus: []*agentv1.GpuInfo{}, Links: []*agentv1.GpuLink{}}
	if g == nil {
		out.UnknownReason = reasonNotReportedSinceMasterStart
	} else {
		out.UnknownReason = g.unknownReason
		out.DriverVersion = g.driverVersion
		if !g.collectedAt.IsZero() {
			out.CollectedAt = protoutils.ToTimestamp(g.collectedAt)
		}
	}

	for _, d := range slots {
		info := aproto.GPUInfo{UUID: d.UUID}
		if g != nil {
			if reported, ok := g.gpus[d.ID]; ok {
				info = reported
			}
		}
		out.Gpus = append(out.Gpus, gpuInfoProto(int32(d.ID), d.UUID, info, false))
	}
	sort.SliceStable(excluded, func(i, j int) bool {
		if excluded[i].PCIBusID != excluded[j].PCIBusID {
			return excluded[i].PCIBusID < excluded[j].PCIBusID
		}
		return excluded[i].UUID < excluded[j].UUID
	})
	for _, info := range excluded {
		out.Gpus = append(out.Gpus, gpuInfoProto(excludedDeviceID, info.UUID, info, true))
	}

	if g == nil {
		return out
	}
	uuidOf := map[device.ID]string{}
	for _, d := range slots {
		uuidOf[d.ID] = d.UUID
	}
	keys := make([]gpuPairKey, 0, len(g.pairs))
	for k := range g.pairs {
		if _, okA := uuidOf[k.a]; okA {
			if _, okB := uuidOf[k.b]; okB {
				keys = append(keys, k)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].a != keys[j].a {
			return keys[i].a < keys[j].a
		}
		return keys[i].b < keys[j].b
	})
	for _, k := range keys {
		p := g.pairs[k]
		out.Links = append(out.Links, gpuLinkProto(int32(k.a), int32(k.b), aproto.GPULink{
			UUIDA: uuidOf[k.a], UUIDB: uuidOf[k.b], Level: p.level, NVLinks: p.nvlinks,
			P2PAToB: p.p2pAToB, P2PBToA: p.p2pBToA,
		}))
	}

	excludedLinks := append([]aproto.GPULink(nil), g.excludedLinks...)
	sort.Slice(excludedLinks, func(i, j int) bool {
		if excludedLinks[i].UUIDA != excludedLinks[j].UUIDA {
			return excludedLinks[i].UUIDA < excludedLinks[j].UUIDA
		}
		return excludedLinks[i].UUIDB < excludedLinks[j].UUIDB
	})
	deviceOf := func(uuid string) int32 {
		if id, ok := slotOf[uuid]; ok {
			return int32(id)
		}
		return excludedDeviceID
	}
	for _, l := range excludedLinks {
		out.Links = append(out.Links, gpuLinkProto(deviceOf(l.UUIDA), deviceOf(l.UUIDB), l))
	}
	return out
}

// gpuHealth returns the health of each CUDA slot by device ID, classified as in the agent API
// (gpuhealth.Apply), from the agent's last report and xids (nil: no XIDs). An excluded GPU is not a
// slot and has no entry.
//
// Only tests call it so far. It is for GPU selection (PR B), which is to rank the slots in
// GPU_HEALTH_ERROR last in allocateFreeDevices. For that, PR B still has to pass the master's
// gpuhealth.XIDCache into the agent RM (agentrm can import gpuhealth, not internal), read its
// LastOK, which never queries Prometheus and survives a failed query, and add a rule for how old
// a result may be and how it is refreshed: the cache queries only when the agent API is asked for
// slots, so without that rule it can be old, or empty after a master restart.
func (a *agentState) gpuHealth(xids *gpuhealth.XIDSnapshot) map[device.ID]agentv1.GpuHealth {
	health := map[device.ID]agentv1.GpuHealth{}
	topo := a.gpuTopologyProto()
	if topo == nil {
		return health
	}
	gpuhealth.Apply(topo, xids)
	for _, g := range topo.Gpus {
		if !g.Excluded {
			health[device.ID(g.DeviceId)] = g.Health
		}
	}
	return health
}

func gpuInfoProto(deviceID int32, uuid string, info aproto.GPUInfo, excluded bool) *agentv1.GpuInfo {
	numa := int32(-1)
	if info.NUMANode != nil && *info.NUMANode >= 0 {
		numa = int32(*info.NUMANode)
	}
	return &agentv1.GpuInfo{
		DeviceId:         deviceID,
		Uuid:             uuid,
		PciBusId:         info.PCIBusID,
		NumaNode:         numa,
		PcieLinkWidth:    int32(info.PCIeLinkWidth),
		PcieLinkWidthMax: int32(info.PCIeLinkWidthMax),
		PcieLinkGen:      int32(info.PCIeLinkGen),
		PcieLinkGenMax:   int32(info.PCIeLinkGenMax),
		NvmlError:        info.NVMLError,
		Excluded:         excluded,
	}
}

func gpuLinkProto(deviceA, deviceB int32, l aproto.GPULink) *agentv1.GpuLink {
	return &agentv1.GpuLink{
		DeviceA: deviceA,
		DeviceB: deviceB,
		UuidA:   l.UUIDA,
		UuidB:   l.UUIDB,
		Level:   gpuLinkLevelProto(l.Level),
		Nvlinks: int32(l.NVLinks),
		P2PAToB: gpuP2PCapsProto(l.P2PAToB),
		P2PBToA: gpuP2PCapsProto(l.P2PBToA),
		P2P:     gpuP2PProto(aproto.P2PUsability(l)),
	}
}

// gpuLinkLevelProto maps a wire level; a string unknown to the master is unknown.
func gpuLinkLevelProto(l aproto.GPULinkLevel) agentv1.GpuLinkLevel {
	switch l {
	case aproto.GPULinkLevelInternal:
		return agentv1.GpuLinkLevel_GPU_LINK_LEVEL_INTERNAL
	case aproto.GPULinkLevelPIX:
		return agentv1.GpuLinkLevel_GPU_LINK_LEVEL_PIX
	case aproto.GPULinkLevelPXB:
		return agentv1.GpuLinkLevel_GPU_LINK_LEVEL_PXB
	case aproto.GPULinkLevelPHB:
		return agentv1.GpuLinkLevel_GPU_LINK_LEVEL_PHB
	case aproto.GPULinkLevelNode:
		return agentv1.GpuLinkLevel_GPU_LINK_LEVEL_NODE
	case aproto.GPULinkLevelSys:
		return agentv1.GpuLinkLevel_GPU_LINK_LEVEL_SYS
	default:
		return agentv1.GpuLinkLevel_GPU_LINK_LEVEL_UNSPECIFIED
	}
}

// gpuP2PStatusProto maps a raw wire status; a string unknown to the master is unknown.
func gpuP2PStatusProto(s aproto.GPUP2PStatus) agentv1.GpuP2PStatus {
	switch s {
	case aproto.GPUP2PStatusOK:
		return agentv1.GpuP2PStatus_GPU_P2P_STATUS_OK
	case aproto.GPUP2PStatusChipsetNotSupported:
		return agentv1.GpuP2PStatus_GPU_P2P_STATUS_CHIPSET_NOT_SUPPORTED
	case aproto.GPUP2PStatusGPUNotSupported:
		return agentv1.GpuP2PStatus_GPU_P2P_STATUS_GPU_NOT_SUPPORTED
	case aproto.GPUP2PStatusTopologyNotSupported:
		return agentv1.GpuP2PStatus_GPU_P2P_STATUS_TOPOLOGY_NOT_SUPPORTED
	case aproto.GPUP2PStatusDisabledByRegkey:
		return agentv1.GpuP2PStatus_GPU_P2P_STATUS_DISABLED_BY_REGKEY
	case aproto.GPUP2PStatusNotSupported:
		return agentv1.GpuP2PStatus_GPU_P2P_STATUS_NOT_SUPPORTED
	default:
		return agentv1.GpuP2PStatus_GPU_P2P_STATUS_UNSPECIFIED
	}
}

func gpuP2PCapsProto(c aproto.GPUP2PCaps) *agentv1.GpuP2PCaps {
	return &agentv1.GpuP2PCaps{Read: gpuP2PStatusProto(c.Read), Write: gpuP2PStatusProto(c.Write)}
}

func gpuP2PProto(u aproto.GPUP2PUsability) agentv1.GpuP2P {
	switch u {
	case aproto.GPUP2PUsable:
		return agentv1.GpuP2P_GPU_P2P_USABLE
	case aproto.GPUP2PNotUsable:
		return agentv1.GpuP2P_GPU_P2P_NOT_USABLE
	default:
		return agentv1.GpuP2P_GPU_P2P_UNSPECIFIED
	}
}
