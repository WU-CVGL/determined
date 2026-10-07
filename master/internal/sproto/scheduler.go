package sproto

import "github.com/determined-ai/determined/master/pkg/schemas/expconf"

// FittingRequirements allow tasks to specify requirements for their placement.
type FittingRequirements struct {
	// SingleAgent specifies that the task must be located within a single agent.
	SingleAgent bool
	// GPUTopology is the task's resources.prefer_gpu_topology; "" and off are off. The agent
	// resource manager reads it only after the fit is chosen.
	GPUTopology expconf.GPUTopologyPreference
}
