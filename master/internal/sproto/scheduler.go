package sproto

import "github.com/determined-ai/determined/master/pkg/schemas/expconf"

// FittingRequirements allow tasks to specify requirements for their placement.
type FittingRequirements struct {
	// SingleAgent specifies that the task must be located within a single agent.
	SingleAgent bool
	// GPUTopology is the task's resources.prefer_gpu_topology; "" and off are off. The agent
	// resource manager reads it only after the fit is chosen.
	GPUTopology expconf.GPUTopologyPreference
	// GPUGroups is the task's resources.gpu_groups, nil when it has none. The agent resource
	// manager places a task with groups only on one agent whose free GPUs hold every group.
	GPUGroups expconf.GPUGroupRequests
}
