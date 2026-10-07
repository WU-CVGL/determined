//go:build integration
// +build integration

package internal

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ghodss/yaml"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	pkgCommand "github.com/determined-ai/determined/master/pkg/command"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// A generic task passes its prefer_gpu_topology to the resource manager; true is refused at
// create.
func TestCreateGenericTaskCarriesGPUTopologyPreference(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	service := &captureAllocationService{}
	oldService := task.DefaultService
	task.DefaultService = service
	t.Cleanup(func() { task.DefaultService = oldService })

	for i, value := range []expconf.GPUTopologyPreference{expconf.GPUTopologySoft, expconf.GPUTopologyStrong} {
		resp, err := api.CreateGenericTask(ctx, &apiv1.CreateGenericTaskRequest{Config: `
entrypoint: ["sleep", "1"]
resources:
  slots: 2
  prefer_gpu_topology: ` + string(value) + "\n"})
		require.NoError(t, err)
		require.Len(t, service.reqs, i+1)
		require.Equal(t, value, service.reqs[i].FittingRequirements.GPUTopology)
		_, spec, err := getGenericTaskSpec(ctx, model.TaskID(resp.TaskId))
		require.NoError(t, err)
		require.Equal(t, value, spec.GenericTaskConfig.Resources.GPUTopology())
		allocationID := service.reqs[i].AllocationID
		t.Cleanup(func() { unregisterGenericTaskJob(spec.JobID, allocationID) })
	}

	_, err := api.CreateGenericTask(ctx, &apiv1.CreateGenericTaskRequest{Config: `
entrypoint: ["sleep", "1"]
resources:
  slots: 2
  prefer_gpu_topology: true
`})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), `prefer_gpu_topology must be false, "soft" or "strong", not true`)
	require.Len(t, service.reqs, 2)
}

// strongValidatingRM is a mock resource manager whose pools have NUMA nodes of 4 slots: it refuses
// a request with prefer_gpu_topology "strong" for more slots, and records every validation.
func strongValidatingRM() (*mocks.ResourceManager, func() []sproto.ValidateResourcesRequest) {
	mockRM := MockRM()
	var kept []*mock.Call
	for _, c := range mockRM.ExpectedCalls {
		if c.Method != "ValidateResources" {
			kept = append(kept, c)
		}
	}
	mockRM.ExpectedCalls = kept
	var mu sync.Mutex
	var seen []sproto.ValidateResourcesRequest
	mockRM.On("ValidateResources", mock.Anything).Return(
		func(req sproto.ValidateResourcesRequest) []pkgCommand.LaunchWarning {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, req)
			return nil
		},
		func(req sproto.ValidateResourcesRequest) error {
			if req.GPUTopology == expconf.GPUTopologyStrong && req.Slots > 4 {
				return errors.New("no NUMA node in pool " + req.ResourcePool + " has 5 slots; use soft")
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

// strongChecks returns the validations that carried prefer_gpu_topology.
func strongChecks(seen []sproto.ValidateResourcesRequest) []sproto.ValidateResourcesRequest {
	var out []sproto.ValidateResourcesRequest
	for _, req := range seen {
		if req.GPUTopology != "" {
			out = append(out, req)
		}
	}
	return out
}

// The paths that create a task check "strong" with the resource manager once the task's config is
// final, template included, and refuse the task when no NUMA node of the pool can hold it.
func TestCreatePathsCheckStrongGPUTopology(t *testing.T) {
	mockRM, seen := strongValidatingRM()
	api, curUser, ctx := setupAPITest(t, nil, mockRM)
	service := &captureAllocationService{}
	oldService := task.DefaultService
	task.DefaultService = service
	t.Cleanup(func() { task.DefaultService = oldService })
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)

	refused := func(err error) {
		t.Helper()
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
		require.Contains(t, err.Error(), "has 5 slots; use soft")
	}

	// Generic tasks.
	generic := func(slots, value string) error {
		resp, err := api.CreateGenericTask(ctx, &apiv1.CreateGenericTaskRequest{Config: `
entrypoint: ["sleep", "1"]
resources:
  slots: ` + slots + `
  prefer_gpu_topology: ` + value + "\n"})
		if err == nil {
			_, spec, err := getGenericTaskSpec(ctx, model.TaskID(resp.TaskId))
			require.NoError(t, err)
			allocationID := service.reqs[len(service.reqs)-1].AllocationID
			t.Cleanup(func() { unregisterGenericTaskJob(spec.JobID, allocationID) })
		}
		return err
	}
	refused(generic("5", "strong"))
	require.NoError(t, generic("4", "strong"))
	require.NoError(t, generic("5", "soft"))
	require.NoError(t, generic("1", "strong"))
	checks := strongChecks(seen())
	require.Len(t, checks, 2, "only strong with 2 or more slots")
	require.Equal(t, sproto.ValidateResourcesRequest{
		ResourcePool: checks[0].ResourcePool, Slots: 5, GPUTopology: expconf.GPUTopologyStrong,
	}, checks[0])

	// Commands, from their config or a template.
	userCtx := ntscUserCtx(t, curUser)
	config, err := structpb.NewStruct(map[string]any{
		"resources": map[string]any{"slots": 5, "prefer_gpu_topology": "strong"},
	})
	require.NoError(t, err)
	_, err = api.LaunchCommand(userCtx, &apiv1.LaunchCommandRequest{Config: config})
	refused(err)

	templateName := "strong-" + uuid.NewString()
	_, err = db.Bun().NewRaw("INSERT INTO templates (name, config, workspace_id) VALUES (?, ?::jsonb, 1)",
		templateName, `{"resources": {"prefer_gpu_topology": "strong"}}`).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Bun().NewDelete().Table("templates").Where("name = ?", templateName).
			Exec(context.Background())
	})
	config, err = structpb.NewStruct(map[string]any{"resources": map[string]any{"slots": 5}})
	require.NoError(t, err)
	_, err = api.LaunchCommand(userCtx, &apiv1.LaunchCommandRequest{
		Config: config, TemplateName: templateName,
	})
	refused(err)

	// Experiments, once templates and invariant config policies are merged.
	experiment := func(slots int) string {
		conf := minExpConfig
		conf.RawResources = &expconf.ResourcesConfig{
			RawResourcePool:      ptrs.Ptr("kubernetes"),
			RawSlotsPerTrial:     ptrs.Ptr(slots),
			RawPreferGPUTopology: ptrs.Ptr(expconf.GPUTopologyStrong),
		}
		bytes, err := yaml.Marshal(conf)
		require.NoError(t, err)
		return string(bytes)
	}
	_, err = api.CreateExperiment(ctx, &apiv1.CreateExperimentRequest{
		Config: experiment(5), ValidateOnly: true,
	})
	refused(err)
	_, err = api.CreateExperiment(ctx, &apiv1.CreateExperimentRequest{
		Config: experiment(4), ValidateOnly: true,
	})
	require.NoError(t, err)
}
