package prom

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// stateSeries returns the values of the exported series of the metric whose labels include all
// of the given labels. Each test uses unique IDs, so tests do not see each other's series.
func stateSeries(t *testing.T, name string, labels map[string]string) []float64 {
	t.Helper()
	families, err := DetStateMetrics.Gather()
	require.NoError(t, err)
	var values []float64
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	metrics:
		for _, metric := range family.GetMetric() {
			found := map[string]string{}
			for _, pair := range metric.GetLabel() {
				found[pair.GetName()] = pair.GetValue()
			}
			for key, value := range labels {
				if found[key] != value {
					continue metrics
				}
			}
			values = append(values, metric.GetGauge().GetValue())
		}
	}
	return values
}

// requireNoZeroSeries checks that the exporter holds no 0-valued or negative series at all.
func requireNoZeroSeries(t *testing.T) {
	t.Helper()
	families, err := DetStateMetrics.Gather()
	require.NoError(t, err)
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			require.Positivef(t, metric.GetGauge().GetValue(), "%s %v", family.GetName(), metric.GetLabel())
		}
	}
}

func testResources() (sproto.ResourcesSummary, *sproto.ResourcesStarted) {
	containerID := cproto.ID(uuid.NewString())
	return sproto.ResourcesSummary{
		AllocationID: model.AllocationID(uuid.NewString()),
		ResourcesID:  sproto.ResourcesID(containerID),
		ContainerID:  &containerID,
		AgentDevices: map[aproto.ID][]device.Device{
			"agent": {
				{UUID: uuid.NewString(), Type: device.CUDA},
				{UUID: uuid.NewString(), Type: device.CUDA},
			},
		},
	}, &sproto.ResourcesStarted{NativeResourcesID: uuid.NewString()}
}

func TestAllocationResourcesLeaveNoSeries(t *testing.T) {
	summary, started := testResources()
	cID := summary.ContainerID.String()
	containerLabels := map[string]string{"container_id": cID}

	AddAllocationResources(summary, started)
	require.Equal(t, []float64{1}, stateSeries(t, "det_container_id_allocation_id", containerLabels))
	require.Equal(t, []float64{1}, stateSeries(t, "det_container_id_runtime_container_id", containerLabels))
	require.Equal(t, []float64{1, 1}, stateSeries(t, "det_gpu_uuid_container_id", containerLabels))

	RemoveAllocationResources(summary, started)
	require.Empty(t, stateSeries(t, "det_container_id_allocation_id", containerLabels))
	require.Empty(t, stateSeries(t, "det_container_id_runtime_container_id", containerLabels))
	require.Empty(t, stateSeries(t, "det_gpu_uuid_container_id", containerLabels))
	requireNoZeroSeries(t)
}

func TestAllocationTaskCountsAssociations(t *testing.T) {
	aID := model.AllocationID(uuid.NewString())
	tID := model.TaskID(uuid.NewString())
	jID := model.JobID(uuid.NewString())
	labels := map[string]string{"allocation_id": aID.String()}
	name := "allocation_id_task_id_task_actor"

	// One association per container of a multi-container allocation.
	AssociateAllocationTask(aID, tID, "trial", jID)
	AssociateAllocationTask(aID, tID, "trial", jID)
	require.Equal(t, []float64{2}, stateSeries(t, "det_"+name, labels))
	DisassociateAllocationTask(aID, tID, "trial", jID)
	require.Equal(t, []float64{1}, stateSeries(t, "det_"+name, labels))
	DisassociateAllocationTask(aID, tID, "trial", jID)
	require.Empty(t, stateSeries(t, "det_"+name, labels))

	// A removal without an association exports nothing, rather than a negative mapping.
	DisassociateAllocationTask(aID, tID, "trial", jID)
	require.Empty(t, stateSeries(t, "det_"+name, labels))
	cID := cproto.ID(uuid.NewString())
	DisassociateAllocationContainer(aID, cID)
	DisassociateContainerGPU(cID, device.Device{UUID: uuid.NewString(), Type: device.CUDA})
	require.Empty(t, stateSeries(t, "det_container_id_allocation_id", map[string]string{"container_id": cID.String()}))
	requireNoZeroSeries(t)
}

func TestJobExperimentLabelsEndWithTheLastTrial(t *testing.T) {
	jID := model.JobID(uuid.NewString())
	eID := uuid.NewString()
	labels := expconf.Labels{"a": true, "b,c": true}
	experiment := map[string]string{"experiment_id": eID}

	// Two trials of one experiment run at once.
	AssociateJobExperiment(jID, eID, labels)
	AssociateJobExperiment(jID, eID, labels)
	require.Equal(t, []float64{2}, stateSeries(t, "det_job_id_experiment_id", experiment))
	require.Equal(t, []float64{2, 2}, stateSeries(t, "det_experiment_id_label", experiment))
	// Editing the experiment's labels replaces them, and both trials keep the edited labels.
	SetExperimentIDLabels(eID, []string{"a", "edited"})
	require.Equal(t, []float64{2, 2}, stateSeries(t, "det_experiment_id_label", experiment))
	require.Empty(t, stateSeries(t, "det_experiment_id_label", map[string]string{
		"experiment_id": eID, "label": "b,c",
	}))

	DisassociateJobExperiment(jID, eID, labels)
	require.Equal(t, []float64{1}, stateSeries(t, "det_job_id_experiment_id", experiment))
	require.ElementsMatch(t, []float64{1, 2}, stateSeries(t, "det_experiment_id_label", experiment))

	DisassociateJobExperiment(jID, eID, labels)
	require.Empty(t, stateSeries(t, "det_job_id_experiment_id", experiment))
	require.Empty(t, stateSeries(t, "det_experiment_id_label", experiment))

	// A removal without an association changes nothing.
	DisassociateJobExperiment(jID, eID, labels)
	require.Empty(t, stateSeries(t, "det_job_id_experiment_id", experiment))
	require.Empty(t, stateSeries(t, "det_experiment_id_label", experiment))
	requireNoZeroSeries(t)
}

// Labels edited while no trial of the experiment has an allocation are not exported, since
// nothing would remove them.
func TestLabelsEditedWithoutTrialsAreNotExported(t *testing.T) {
	eID := uuid.NewString()
	SetExperimentIDLabels(eID, []string{"edited"})
	require.Empty(t, stateSeries(t, "det_experiment_id_label", map[string]string{"experiment_id": eID}))
}

func TestMappingsConcurrently(t *testing.T) {
	jID := model.JobID(uuid.NewString())
	eID := uuid.NewString()
	aID := model.AllocationID(uuid.NewString())
	tID := model.TaskID(uuid.NewString())
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				summary, started := testResources()
				summary.AllocationID = aID
				AssociateAllocationTask(aID, tID, "trial", jID)
				AddAllocationResources(summary, started)
				AssociateJobExperiment(jID, eID, expconf.Labels{"x": true})
				DisassociateJobExperiment(jID, eID, expconf.Labels{"x": true})
				RemoveAllocationResources(summary, started)
				DisassociateAllocationTask(aID, tID, "trial", jID)
			}
		}()
	}
	wg.Wait()
	allocation := map[string]string{"allocation_id": aID.String()}
	experiment := map[string]string{"experiment_id": eID}
	require.Empty(t, stateSeries(t, "det_allocation_id_task_id_task_actor", allocation))
	require.Empty(t, stateSeries(t, "det_container_id_allocation_id", allocation))
	require.Empty(t, stateSeries(t, "det_job_id_experiment_id", experiment))
	require.Empty(t, stateSeries(t, "det_experiment_id_label", experiment))
	requireNoZeroSeries(t)
}
