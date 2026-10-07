//go:build integration
// +build integration

package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// A generic task passes its prefer_gpu_topology to the resource manager; true and "strong" are
// refused at create.
func TestCreateGenericTaskCarriesGPUTopologyPreference(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	service := &captureAllocationService{}
	oldService := task.DefaultService
	task.DefaultService = service
	t.Cleanup(func() { task.DefaultService = oldService })

	resp, err := api.CreateGenericTask(ctx, &apiv1.CreateGenericTaskRequest{Config: `
entrypoint: ["sleep", "1"]
resources:
  slots: 2
  prefer_gpu_topology: soft
`})
	require.NoError(t, err)
	require.Len(t, service.reqs, 1)
	require.Equal(t, expconf.GPUTopologySoft, service.reqs[0].FittingRequirements.GPUTopology)
	_, spec, err := getGenericTaskSpec(ctx, model.TaskID(resp.TaskId))
	require.NoError(t, err)
	require.Equal(t, expconf.GPUTopologySoft, spec.GenericTaskConfig.Resources.GPUTopology())
	t.Cleanup(func() { unregisterGenericTaskJob(spec.JobID, service.reqs[0].AllocationID) })

	for value, want := range map[string]string{
		"true":   `prefer_gpu_topology must be false, "soft" or "strong", not true`,
		"strong": `prefer_gpu_topology "strong" is not available yet; use "soft"`,
	} {
		_, err := api.CreateGenericTask(ctx, &apiv1.CreateGenericTaskRequest{Config: `
entrypoint: ["sleep", "1"]
resources:
  slots: 2
  prefer_gpu_topology: ` + value + "\n"})
		require.Equal(t, codes.InvalidArgument, status.Code(err), value)
		require.Contains(t, err.Error(), want, value)
	}
	require.Len(t, service.reqs, 1)
}
