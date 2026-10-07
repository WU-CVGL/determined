package expconf

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// GPUTopologyPreference is resources.prefer_gpu_topology: how the agent resource manager chooses a
// task's GPUs by the GPU topology that agents report. In JSON, off is false; soft and strong are
// the strings "soft" and "strong". true is not a value, so it can never silently mean one mode.
type GPUTopologyPreference string

const (
	// GPUTopologyOff chooses GPUs as for tasks without the preference.
	GPUTopologyOff GPUTopologyPreference = "off"
	// GPUTopologySoft chooses the best-ranked set among the free GPUs of the agent that the
	// scheduler picks. It never waits and never changes which agent the task gets.
	GPUTopologySoft GPUTopologyPreference = "soft"
	// GPUTopologyStrong waits for one NUMA node of an agent to hold the task. It is not available
	// yet: Validate rejects it.
	GPUTopologyStrong GPUTopologyPreference = "strong"
)

// MarshalJSON writes false for off.
func (p GPUTopologyPreference) MarshalJSON() ([]byte, error) {
	switch p {
	case GPUTopologyOff:
		return []byte("false"), nil
	case GPUTopologySoft, GPUTopologyStrong:
		return json.Marshal(string(p))
	default:
		return nil, fmt.Errorf("invalid prefer_gpu_topology %q", string(p))
	}
}

// UnmarshalJSON reads false as off, and "soft" and "strong"; it rejects true and anything else.
// null leaves the value unchanged, as for other types.
func (p *GPUTopologyPreference) UnmarshalJSON(data []byte) error {
	switch trimmed := bytes.TrimSpace(data); string(trimmed) {
	case "null":
		return nil
	case "false":
		*p = GPUTopologyOff
		return nil
	case `"soft"`:
		*p = GPUTopologySoft
		return nil
	case `"strong"`:
		*p = GPUTopologyStrong
		return nil
	default:
		return fmt.Errorf(
			`prefer_gpu_topology must be false, "soft" or "strong", not %s`, string(trimmed))
	}
}

// Validate implements check.Validatable: "strong" is not available yet.
func (p GPUTopologyPreference) Validate() []error {
	if p == GPUTopologyStrong {
		return []error{fmt.Errorf(`prefer_gpu_topology "strong" is not available yet; use "soft"`)}
	}
	return nil
}

// GPUTopology returns resources.prefer_gpu_topology, off when it is not set. Generic tasks never
// get WithDefaults, so it is nil-safe.
func (r ResourcesConfigV0) GPUTopology() GPUTopologyPreference {
	if r.RawPreferGPUTopology == nil {
		return GPUTopologyOff
	}
	return *r.RawPreferGPUTopology
}
