//go:build integration
// +build integration

package command

import (
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

var testGPUGroups = expconf.GPUGroupsConfig{
	{RawName: "simulation", RawCount: 1, RawModels: []string{"NVIDIA GeForce RTX 4090"}},
	{RawName: "learner", RawCount: 1, RawModels: []string{"NVIDIA RTX 6000 Ada Generation"}},
}

// Commands, notebooks, shells and TensorBoards pass their gpu_groups to the resource manager.
func TestNTSCCarriesGPUGroups(t *testing.T) {
	pgDB := setupTest(t)
	starts := &captureStarts{}
	old := task.DefaultService
	task.DefaultService = starts
	t.Cleanup(func() { task.DefaultService = old })

	for _, kind := range []struct {
		task model.TaskType
		job  model.JobType
	}{
		{model.TaskTypeCommand, model.JobTypeCommand},
		{model.TaskTypeNotebook, model.JobTypeNotebook},
		{model.TaskTypeShell, model.JobTypeShell},
		{model.TaskTypeTensorboard, model.JobTypeTensorboard},
	} {
		req := CreateMockGenericReq(t, pgDB)
		req.Spec.Config.Resources.Slots = 2
		req.Spec.Config.Resources.GPUGroups = testGPUGroups
		_, err := DefaultCmdService.LaunchGenericCommand(kind.task, kind.job, req)
		require.NoError(t, err)

		starts.mu.Lock()
		got := starts.reqs[len(starts.reqs)-1].FittingRequirements
		starts.mu.Unlock()
		require.True(t, got.SingleAgent)
		require.Equal(t, testGPUGroups.Requests(), got.GPUGroups, kind.task)
	}
}

// A shell with gpu_groups that waits for resources when the master stops keeps its groups after
// the master restarts: its command snapshot carries them to the new allocation request.
func TestRestoreQueuedShellKeepsGPUGroups(t *testing.T) {
	shell, restartedRM, _ := restartWithQueuedShell(t, time.Now().UTC().Add(-time.Hour), func(req *CreateGeneric) {
		req.Spec.Config.Resources.Slots = 2
		req.Spec.Config.Resources.GPUGroups = testGPUGroups
	})
	require.Equal(t, testGPUGroups.Requests(), shell.Config.Resources.GPUGroupRequests())
	restartedRM.AssertCalled(t, "Allocate", mock.MatchedBy(func(req sproto.AllocateRequest) bool {
		return req.AllocationID == shell.allocationID && req.SlotsNeeded == 2 &&
			len(req.FittingRequirements.GPUGroups) == 2 &&
			req.FittingRequirements.GPUGroups[1].Name == "learner"
	}))
}
