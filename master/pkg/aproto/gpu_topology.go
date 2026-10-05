package aproto

import "time"

// GPU topology wire format (agent to master). Agents collect it with NVML once, at process start.
// The zero value of every enum is "", which means unknown. Every field except UUID is omitempty,
// and 0 or nil means unknown. A master that does not know an enum string treats it as unknown.

// GPULinkLevel is the closest common ancestor of two GPUs in the PCIe tree, as NVML reports it.
type GPULinkLevel string

// GPULinkLevel values, from closest to farthest. "" means unknown.
const (
	GPULinkLevelInternal GPULinkLevel = "INTERNAL"
	GPULinkLevelPIX      GPULinkLevel = "PIX"
	GPULinkLevelPXB      GPULinkLevel = "PXB"
	GPULinkLevelPHB      GPULinkLevel = "PHB"
	GPULinkLevelNode     GPULinkLevel = "NODE"
	GPULinkLevelSys      GPULinkLevel = "SYS"
)

// Known reports whether the level is one of the defined values.
func (l GPULinkLevel) Known() bool {
	switch l {
	case GPULinkLevelInternal, GPULinkLevelPIX, GPULinkLevelPXB, GPULinkLevelPHB,
		GPULinkLevelNode, GPULinkLevelSys:
		return true
	default:
		return false
	}
}

// GPUP2PStatus is one raw NVML P2P status (READ or WRITE, one direction).
type GPUP2PStatus string

// GPUP2PStatus values. "" means unknown: the query failed, or NVML answered P2P_STATUS_UNKNOWN.
const (
	GPUP2PStatusOK                   GPUP2PStatus = "OK"
	GPUP2PStatusChipsetNotSupported  GPUP2PStatus = "CHIPSET_NOT_SUPPORTED"
	GPUP2PStatusGPUNotSupported      GPUP2PStatus = "GPU_NOT_SUPPORTED"
	GPUP2PStatusTopologyNotSupported GPUP2PStatus = "TOPOLOGY_NOT_SUPPORTED"
	GPUP2PStatusDisabledByRegkey     GPUP2PStatus = "DISABLED_BY_REGKEY"
	GPUP2PStatusNotSupported         GPUP2PStatus = "NOT_SUPPORTED"
)

// Known reports whether the status is one of the defined values. Any other string, including
// one a newer agent may send, is unknown.
func (s GPUP2PStatus) Known() bool {
	switch s {
	case GPUP2PStatusOK, GPUP2PStatusChipsetNotSupported, GPUP2PStatusGPUNotSupported,
		GPUP2PStatusTopologyNotSupported, GPUP2PStatusDisabledByRegkey, GPUP2PStatusNotSupported:
		return true
	default:
		return false
	}
}

// GPUP2PCaps holds the raw READ and WRITE P2P statuses of one direction.
type GPUP2PCaps struct {
	Read  GPUP2PStatus `json:"read,omitempty"`
	Write GPUP2PStatus `json:"write,omitempty"`
}

// GPUInfo describes one GPU: a slot, or a GPU left out by the agent's exclude list. UUID and
// Excluded come from device detection; the other fields from NVML, at agent start.
type GPUInfo struct {
	UUID             string `json:"uuid"`
	PCIBusID         string `json:"pci_bus_id,omitempty"`
	NUMANode         *int   `json:"numa_node,omitempty"`
	PCIeLinkWidth    int    `json:"pcie_link_width,omitempty"`
	PCIeLinkWidthMax int    `json:"pcie_link_width_max,omitempty"`
	PCIeLinkGen      int    `json:"pcie_link_gen,omitempty"`
	PCIeLinkGenMax   int    `json:"pcie_link_gen_max,omitempty"`
	// NVMLError lists the NVML health calls that failed, as "<call>: <NAME> (<code>)", joined
	// with "; ".
	NVMLError string `json:"nvml_error,omitempty"`
	// Excluded is set for a GPU left out by the agent's exclude list. It is never a slot.
	Excluded bool `json:"excluded,omitempty"`
}

// GPULink describes one unordered pair of GPUs, with UUIDA < UUIDB.
type GPULink struct {
	UUIDA   string       `json:"uuid_a,omitempty"`
	UUIDB   string       `json:"uuid_b,omitempty"`
	Level   GPULinkLevel `json:"level,omitempty"`
	NVLinks int          `json:"nvlinks,omitempty"`
	P2PAToB GPUP2PCaps   `json:"p2p_a_to_b,omitempty"`
	P2PBToA GPUP2PCaps   `json:"p2p_b_to_a,omitempty"`
}

// GPUTopology is what the agent measured at process start. When UnknownReason is set, GPUs is
// the inventory from device detection (UUID and Excluded only) and Links is empty.
type GPUTopology struct {
	UnknownReason string `json:"unknown_reason,omitempty"`
	// CollectedAt is nil when unknown. It is a pointer because encoding/json never omits a struct
	// value such as a zero time.Time.
	CollectedAt   *time.Time `json:"collected_at,omitempty"`
	DriverVersion string     `json:"driver_version,omitempty"`
	GPUs          []GPUInfo  `json:"gpus,omitempty"`
	Links         []GPULink  `json:"links,omitempty"`
}

// GPUP2PUsability is the P2P state of a pair of GPUs derived from its four raw statuses.
type GPUP2PUsability string

// GPUP2PUsability values. "" means unknown.
const (
	GPUP2PUsable    GPUP2PUsability = "USABLE"
	GPUP2PNotUsable GPUP2PUsability = "NOT_USABLE"
)

// P2PUsability derives a pair's P2P state. One direction is usable when its READ and WRITE are
// both OK, as NCCL requires.
//   - USABLE: all four statuses are OK, so both directions are usable;
//   - NOT_USABLE: at least one of the four is a known status other than OK;
//   - unknown (""): otherwise, that is, a failed query or an unknown status and no known non-OK
//     status.
func P2PUsability(l GPULink) GPUP2PUsability {
	statuses := [4]GPUP2PStatus{l.P2PAToB.Read, l.P2PAToB.Write, l.P2PBToA.Read, l.P2PBToA.Write}
	allOK := true
	for _, s := range statuses {
		if s.Known() && s != GPUP2PStatusOK {
			return GPUP2PNotUsable
		}
		if s != GPUP2PStatusOK {
			allOK = false
		}
	}
	if allOK {
		return GPUP2PUsable
	}
	return ""
}
