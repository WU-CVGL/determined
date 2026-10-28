//go:build integration
// +build integration

package internal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ghodss/yaml"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/configpolicy"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	pkgCommand "github.com/determined-ai/determined/master/pkg/command"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/searcher"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

const (
	validateResourcesMethod = "ValidateResources"

	h100     = "NVIDIA H100 PCIe"
	rtx4090  = "NVIDIA GeForce RTX 4090"
	ada6000  = "NVIDIA RTX 6000 Ada Generation"
	simGroup = `{"name": "sim", "count": 1, "models": ["` + rtx4090 + `"]}`
)

// gpuGroupsConfig is a resources.gpu_groups list as JSON: one GPU for simulation and one learner
// of the model learner.
func gpuGroupsConfig(learner string) string {
	return `[` + simGroup + `, {"name": "learner", "count": 1, "models": ["` + learner + `"]}]`
}

// groupsValidatingRM is a mock resource manager whose pools have no H100: it refuses gpu_groups
// that need one, as the agent resource manager does, and records every validation.
func groupsValidatingRM() (*mocks.ResourceManager, func() []sproto.ValidateResourcesRequest) {
	mockRM := MockRM()
	var kept []*mock.Call
	for _, c := range mockRM.ExpectedCalls {
		if c.Method != validateResourcesMethod {
			kept = append(kept, c)
		}
	}
	mockRM.ExpectedCalls = kept
	var mu sync.Mutex
	var seen []sproto.ValidateResourcesRequest
	mockRM.On(validateResourcesMethod, mock.Anything).Return(
		func(req sproto.ValidateResourcesRequest) []pkgCommand.LaunchWarning {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, req)
			return nil
		},
		func(req sproto.ValidateResourcesRequest) error {
			for _, g := range req.GPUGroups {
				if slices.Contains(g.Models, h100) {
					return errors.New("no agent in pool " + req.ResourcePool + " has GPUs for gpu_groups " +
						req.GPUGroups.String())
				}
			}
			return nil
		},
	)
	mockRM.On("SmallerValueIsHigherPriority", mock.Anything).Return(true, nil)
	return mockRM, func() []sproto.ValidateResourcesRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]sproto.ValidateResourcesRequest(nil), seen...)
	}
}

// groupsChecks returns the validations that carried gpu_groups.
func groupsChecks(seen []sproto.ValidateResourcesRequest) []sproto.ValidateResourcesRequest {
	var out []sproto.ValidateResourcesRequest
	for _, req := range seen {
		if req.GPUGroups != nil {
			out = append(out, req)
		}
	}
	return out
}

var wantGroups = expconf.GPUGroupRequests{
	{Name: "sim", Count: 1, Models: []string{rtx4090}},
	{Name: "learner", Count: 1, Models: []string{ada6000}},
}

func requireInvalidArgument(t *testing.T, err error, msg string) {
	t.Helper()
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	require.ErrorContains(t, err, msg)
}

// The paths that create a task refuse gpu_groups whose counts do not add up to the task's slots
// and groups that no agent of the pool can hold, once the config is final; tasks carry the groups
// to the resource manager.
func TestCreatePathsCheckGPUGroups(t *testing.T) {
	mockRM, seen := groupsValidatingRM()
	api, curUser, ctx := setupAPITest(t, nil, mockRM)
	service := &captureAllocationService{}
	oldService := task.DefaultService
	task.DefaultService = service
	t.Cleanup(func() { task.DefaultService = oldService })
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	impossible := "has GPUs for gpu_groups sim: 1 × " + rtx4090 + "; learner: 1 × " + h100

	// Generic tasks.
	generic := func(slots, groups string) error {
		resp, err := api.CreateGenericTask(ctx, &apiv1.CreateGenericTaskRequest{Config: `
entrypoint: ["sleep", "1"]
resources:
  slots: ` + slots + `
  gpu_groups: ` + groups + "\n"})
		if err == nil {
			_, spec, err := getGenericTaskSpec(ctx, model.TaskID(resp.TaskId))
			require.NoError(t, err)
			require.Equal(t, wantGroups, spec.GenericTaskConfig.Resources.GPUGroupRequests())
			allocationID := service.reqs[len(service.reqs)-1].AllocationID
			t.Cleanup(func() { unregisterGenericTaskJob(spec.JobID, allocationID) })
		}
		return err
	}
	requireInvalidArgument(t, generic("3", gpuGroupsConfig(ada6000)),
		"resources.gpu_groups counts must add up to resources.slots (3)")
	requireInvalidArgument(t, generic("2", `[`+simGroup+`, `+simGroup+`]`),
		`resources.gpu_groups name "sim" is not unique`)
	requireInvalidArgument(t, generic("2", gpuGroupsConfig(h100)), impossible)
	require.NoError(t, generic("2", gpuGroupsConfig(ada6000)))
	require.Equal(t, wantGroups, service.reqs[len(service.reqs)-1].FittingRequirements.GPUGroups)
	checks := groupsChecks(seen())
	require.Equal(t, sproto.ValidateResourcesRequest{
		ResourcePool: checks[len(checks)-1].ResourcePool, Slots: 2, GPUGroups: wantGroups,
	}, checks[len(checks)-1])

	// Commands, notebooks, shells and TensorBoards, from their config or a template.
	userCtx := ntscUserCtx(t, curUser)
	ntscConfig := func(slots int, groups string) *structpb.Struct {
		var list []any
		require.NoError(t, yaml.Unmarshal([]byte(groups), &list))
		resources := map[string]any{"gpu_groups": list}
		if slots >= 0 {
			resources["slots"] = slots
		}
		config, err := structpb.NewStruct(map[string]any{"resources": resources})
		require.NoError(t, err)
		return config
	}
	_, err = api.LaunchCommand(userCtx, &apiv1.LaunchCommandRequest{Config: ntscConfig(3, gpuGroupsConfig(ada6000))})
	requireInvalidArgument(t, err, "resources.gpu_groups counts must add up to resources.slots (3)")
	_, err = api.LaunchCommand(userCtx, &apiv1.LaunchCommandRequest{Config: ntscConfig(2, gpuGroupsConfig(h100))})
	requireInvalidArgument(t, err, impossible)
	_, err = api.LaunchNotebook(userCtx, &apiv1.LaunchNotebookRequest{Config: ntscConfig(1, gpuGroupsConfig(ada6000))})
	requireInvalidArgument(t, err, "resources.gpu_groups counts must add up to resources.slots (1)")
	_, err = api.LaunchShell(userCtx, &apiv1.LaunchShellRequest{Config: ntscConfig(4, gpuGroupsConfig(ada6000))})
	requireInvalidArgument(t, err, "resources.gpu_groups counts must add up to resources.slots (4)")
	_, err = api.LaunchTensorboard(userCtx, &apiv1.LaunchTensorboardRequest{
		ExperimentIds: []int32{1}, Config: ntscConfig(-1, gpuGroupsConfig(ada6000)),
	})
	requireInvalidArgument(t, err, "resources.gpu_groups counts must add up to resources.slots (0)")

	templateName := "groups-" + uuid.NewString()
	_, err = db.Bun().NewRaw("INSERT INTO templates (name, config, workspace_id) VALUES (?, ?::jsonb, 1)",
		templateName, `{"resources": {"gpu_groups": `+gpuGroupsConfig(h100)+`}}`).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Bun().NewDelete().Table("templates").Where("name = ?", templateName).
			Exec(context.Background())
	})
	config, err := structpb.NewStruct(map[string]any{"resources": map[string]any{"slots": 2}})
	require.NoError(t, err)
	_, err = api.LaunchCommand(userCtx, &apiv1.LaunchCommandRequest{Config: config, TemplateName: templateName})
	requireInvalidArgument(t, err, impossible)

	// Experiments, once templates and invariant config policies are merged.
	experiment := func(slots int, learner string) string {
		conf := minExpConfig
		var groups expconf.GPUGroupsConfig
		require.NoError(t, yaml.Unmarshal([]byte(gpuGroupsConfig(learner)), &groups))
		conf.RawResources = &expconf.ResourcesConfig{
			RawResourcePool: ptrs.Ptr("kubernetes"), RawSlotsPerTrial: ptrs.Ptr(slots), RawGPUGroups: groups,
		}
		bytes, err := yaml.Marshal(conf)
		require.NoError(t, err)
		return string(bytes)
	}
	_, err = api.CreateExperiment(ctx, &apiv1.CreateExperimentRequest{
		Config: experiment(1, ada6000), ValidateOnly: true,
	})
	requireInvalidArgument(t, err, "resources.gpu_groups counts must add up to resources.slots_per_trial (1)")
	_, err = api.CreateExperiment(ctx, &apiv1.CreateExperimentRequest{
		Config: experiment(2, h100), ValidateOnly: true,
	})
	requireInvalidArgument(t, err,
		"no agent in pool kubernetes has GPUs for gpu_groups sim: 1 × "+rtx4090+"; learner: 1 × "+h100)
	_, err = api.CreateExperiment(ctx, &apiv1.CreateExperimentRequest{
		Config: experiment(2, ada6000), ValidateOnly: true,
	})
	require.NoError(t, err)

	// An invariant config policy that forces the groups and the pool: the counts are checked
	// against the experiment's slots and the pool it runs in.
	plain := func(slots int) string {
		conf := minExpConfig
		conf.RawResources = &expconf.ResourcesConfig{
			RawResourcePool: ptrs.Ptr("kubernetes"), RawSlotsPerTrial: ptrs.Ptr(slots),
		}
		bytes, err := yaml.Marshal(conf)
		require.NoError(t, err)
		return string(bytes)
	}
	wsID, _ := db.RequireMockWorkspaceID(t, api.m.db, "")
	projID, _ := db.RequireMockProjectID(t, api.m.db, wsID, false)
	require.NoError(t, configpolicy.SetTaskConfigPolicies(ctx, &model.TaskConfigPolicies{
		WorkspaceID: &wsID, WorkloadType: model.ExperimentType, LastUpdatedBy: curUser.ID,
		InvariantConfig: ptrs.Ptr(`{"resources": {"gpu_groups": ` + gpuGroupsConfig(ada6000) +
			`, "resource_pool": "forced"}}`),
	}))
	_, err = api.CreateExperiment(ctx, &apiv1.CreateExperimentRequest{
		Config: plain(4), ProjectId: int32(projID), ValidateOnly: true,
	})
	requireInvalidArgument(t, err, "resources.gpu_groups counts must add up to resources.slots_per_trial (4)")
	_, err = api.CreateExperiment(ctx, &apiv1.CreateExperimentRequest{
		Config: plain(2), ProjectId: int32(projID), ValidateOnly: true,
	})
	require.NoError(t, err)
	checks = groupsChecks(seen())
	require.Equal(t, sproto.ValidateResourcesRequest{
		ResourcePool: "forced", Slots: 2, GPUGroups: wantGroups,
	}, checks[len(checks)-1])
}

// Continuing an experiment checks gpu_groups against the config it would run with, and so does
// moving an experiment to another pool. A stored config keeps the groups.
func TestContinueAndMoveCheckGPUGroups(t *testing.T) {
	mockRM, seen := groupsValidatingRM()
	api, curUser, _ := setupAPITest(t, nil, mockRM)

	owner := addContinueTestUser(t, false, 51400)
	expID := endedTestExpInProject(t, api, owner, model.DefaultProjectID,
		"resources: {resource_pool: kubernetes, slots_per_trial: 2, gpu_groups: "+gpuGroupsConfig(h100)+"}")
	stored, err := api.m.db.ActiveExperimentConfig(expID)
	require.NoError(t, err)
	require.Equal(t, h100, stored.Resources().GPUGroupRequests()[1].Models[0])
	_, err = api.ContinueExperiment(owner.ctx, &apiv1.ContinueExperimentRequest{Id: int32(expID)})
	requireInvalidArgument(t, err, "no agent in pool kubernetes has GPUs for gpu_groups")

	move := func(learner string) (*internalExperiment, error) {
		cfg, err := expconf.ParseAnyExperimentConfigYAML([]byte(
			"resources: {resource_pool: kubernetes, slots_per_trial: 2, gpu_groups: " + gpuGroupsConfig(learner) + "}"))
		require.NoError(t, err)
		active := schemas.WithDefaults(schemas.Merge(cfg, minExpConfig))
		exp := createTestExpWithActiveConfig(t, api, curUser, model.DefaultProjectID, active)
		e := &internalExperiment{
			Experiment: exp, activeConfig: active, db: api.m.db, rm: api.m.rm,
			trials: map[model.RequestID]*trial{},
		}
		return e, e.setRP("other")
	}
	e, err := move(h100)
	requireInvalidArgument(t, err, "no agent in pool other has GPUs for gpu_groups")
	require.Equal(t, "kubernetes", e.activeConfig.Resources().ResourcePool())
	e, err = move(ada6000)
	require.NoError(t, err)
	require.Equal(t, "other", e.activeConfig.Resources().ResourcePool())
	stored, err = api.m.db.ActiveExperimentConfig(e.ID)
	require.NoError(t, err)
	require.Equal(t, wantGroups, stored.Resources().GPUGroupRequests(), "a stored config keeps the groups")

	checks := groupsChecks(seen())
	require.Equal(t, sproto.ValidateResourcesRequest{
		ResourcePool: "other", Slots: 2, GPUGroups: wantGroups,
	}, checks[len(checks)-1])
}

// A trial passes its gpu_groups to the resource manager, for a new allocation and for one restored
// after a master restart from the stored experiment config.
func TestTrialCarriesGPUGroups(t *testing.T) {
	var groups expconf.GPUGroupsConfig
	require.NoError(t, yaml.Unmarshal([]byte(gpuGroupsConfig(ada6000)), &groups))
	for _, restored := range []bool{false, true} {
		_, tr, alloc, _ := setupWithResources(t, &expconf.ResourcesConfig{
			RawSlotsPerTrial: ptrs.Ptr(2), RawGPUGroups: groups,
		})
		if restored {
			tr.rm.(*mocks.ResourceManager).On("IsReattachableOnlyAfterStarted").Return(false)
			require.NoError(t, db.AddAllocation(context.TODO(), &model.Allocation{
				AllocationID: model.AllocationID(fmt.Sprintf("%s.1", tr.taskID)),
				TaskID:       tr.taskID,
				Slots:        2,
				ResourcePool: "default",
				StartTime:    ptrs.Ptr(time.Now().UTC()),
				State:        ptrs.Ptr(model.AllocationStateRunning),
				Ports:        map[string]int{},
			}))
			tr.restored = true
		}
		alloc.ExpectedCalls = nil
		var req sproto.AllocateRequest
		alloc.On(
			"StartAllocation", mock.Anything, mock.Anything, mock.Anything,
			mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		).Run(func(args mock.Arguments) {
			req = args.Get(1).(sproto.AllocateRequest)
		}).Return(nil)

		require.NoError(t, tr.PatchState(model.StateWithReason{State: model.ActiveState}))
		require.NoError(t, tr.PatchSearcherState(experiment.TrialSearcherState{Create: searcher.Create{}}))
		require.Equal(t, restored, req.Restore)
		require.Equal(t, 2, req.SlotsNeeded)
		require.Equal(t, wantGroups, req.FittingRequirements.GPUGroups)
	}
}
