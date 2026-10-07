//go:build integration
// +build integration

package command

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/logger"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// captureStarts records the allocation requests of launched tasks.
type captureStarts struct {
	task.AllocationService
	mu   sync.Mutex
	reqs []sproto.AllocateRequest
}

func (s *captureStarts) StartAllocation(
	_ logger.Context, req sproto.AllocateRequest, _ db.DB, _ rm.ResourceManager,
	_ tasks.TaskSpecifier, _ func(*task.AllocationExited),
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	now := time.Now().UTC()
	return db.AddAllocation(context.Background(), &model.Allocation{
		AllocationID: req.AllocationID, TaskID: req.TaskID, Slots: req.SlotsNeeded,
		ResourcePool: req.ResourcePool, StartTime: &now, Ports: map[string]int{},
	})
}

// Commands, notebooks, shells and TensorBoards pass their prefer_gpu_topology to the resource
// manager.
func TestNTSCCarriesGPUTopologyPreference(t *testing.T) {
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
		for _, pref := range []*expconf.GPUTopologyPreference{nil, ptrs.Ptr(expconf.GPUTopologySoft)} {
			req := CreateMockGenericReq(t, pgDB)
			req.Spec.Config.Resources.Slots = 2
			req.Spec.Config.Resources.PreferGPUTopology = pref
			_, err := DefaultCmdService.LaunchGenericCommand(kind.task, kind.job, req)
			require.NoError(t, err)

			starts.mu.Lock()
			got := starts.reqs[len(starts.reqs)-1].FittingRequirements
			starts.mu.Unlock()
			require.True(t, got.SingleAgent)
			want := expconf.GPUTopologyOff
			if pref != nil {
				want = *pref
			}
			require.Equal(t, want, got.GPUTopology, kind.task)
		}
	}
}
