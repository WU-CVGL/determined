//go:build integration
// +build integration

package kubernetesrm

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	batchV1 "k8s.io/api/batch/v1"
	k8sV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedBatchV1 "k8s.io/client-go/kubernetes/typed/batch/v1"
	typedV1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/rm/rmevents"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

// restorePool returns a resource pool whose cluster has the job of the allocation running, or no
// job when none is given, and the ID of a job with max_slots 1.
func restorePool(t *testing.T, running ...model.AllocationID) (*kubernetesResourcePool, model.JobID) {
	const namespace = "restored"
	jobList, configMapList := &batchV1.JobList{}, &k8sV1.ConfigMapList{}
	for _, id := range running {
		jobList.Items = append(jobList.Items, batchV1.Job{ObjectMeta: metaV1.ObjectMeta{
			Name:      string(id),
			Namespace: namespace,
			Labels: map[string]string{
				determinedLabel:   string(id),
				resourcePoolLabel: defaultResourcePool,
			},
		}})
		configMapList.Items = append(configMapList.Items, k8sV1.ConfigMap{
			ObjectMeta: metaV1.ObjectMeta{Name: string(id), Namespace: namespace},
		})
	}
	jobs := &mocks.JobInterface{}
	jobs.On("List", mock.Anything, mock.Anything).Return(jobList, nil)
	configMaps := &mocks.ConfigMapInterface{}
	configMaps.On("List", mock.Anything, mock.Anything).Return(configMapList, nil)
	pods := &mocks.PodInterface{}
	pods.On("List", mock.Anything, mock.Anything).Return(&k8sV1.PodList{}, nil)
	j := &jobsService{
		namespace:                         defaultNamespace,
		syslog:                            logrus.WithField("component", "jobs"),
		jobInterfaces:                     map[string]typedBatchV1.JobInterface{"": jobs, namespace: jobs},
		configMapInterfaces:               map[string]typedV1.ConfigMapInterface{namespace: configMaps},
		podInterfaces:                     map[string]typedV1.PodInterface{"": pods, namespace: pods},
		jobNameToJobHandler:               map[string]*job{},
		jobNameToResourcePool:             map[string]string{},
		jobNameToPodNameToSchedulingState: map[string]map[string]sproto.SchedulingState{},
		allocationIDToJobName:             map[model.AllocationID]string{},
		jobHandlerToMetadata:              map[*job]jobMetadata{},
	}
	rp := newTestResourcePool(j)

	jobID := model.NewJobID()
	require.NoError(t, tasklist.GroupPriorityChangeRegistry.Add(jobID, func(int) error { return nil }))
	t.Cleanup(func() { _ = tasklist.GroupPriorityChangeRegistry.Delete(jobID) })
	rp.SetGroupMaxSlots(sproto.SetGroupMaxSlots{MaxSlots: ptrs.Ptr(1), JobID: jobID})
	return rp, jobID
}

var restoreTestSubmitted = time.Now().Add(-time.Hour)

func restoreTestRequest(
	jobID model.JobID, id model.AllocationID, requested time.Time, restore bool,
) sproto.AllocateRequest {
	return sproto.AllocateRequest{
		AllocationID:      id,
		TaskID:            model.TaskID(id),
		JobID:             jobID,
		JobSubmissionTime: restoreTestSubmitted,
		RequestTime:       requested,
		IsUserVisible:     true,
		Name:              string(id),
		SlotsNeeded:       1,
		ResourcePool:      defaultResourcePool,
		Restore:           restore,
	}
}

// After a master restart, a trial that was running reattaches to its pods although a trial of the
// same experiment that was queued requested resources first; the queued one waits for max_slots.
func TestRestoreAdmittedBeforeQueued(t *testing.T) {
	running, queued := model.AllocationID(uuid.NewString()), model.AllocationID(uuid.NewString())
	rp, jobID := restorePool(t, running)
	rp.AllocateRequest(restoreTestRequest(jobID, queued, time.Now().Add(-time.Second), false))
	rp.AllocateRequest(restoreTestRequest(jobID, running, time.Now(), true))
	rp.Admit()
	require.Len(t, rp.GetAllocationSummary(running).Resources, 1)
	require.Empty(t, rp.GetAllocationSummary(queued).Resources)

	rp.ResourcesReleased(sproto.ResourcesReleased{AllocationID: running})
	rp.Admit()
	require.Len(t, rp.GetAllocationSummary(queued).Resources, 1)
}

// A trial that was running reattaches to its pods also when a queued trial of the same experiment
// was admitted before its restore was requested.
func TestRestoreNotHeldByMaxSlots(t *testing.T) {
	running, queued := model.AllocationID(uuid.NewString()), model.AllocationID(uuid.NewString())
	rp, jobID := restorePool(t, running)
	rp.AllocateRequest(restoreTestRequest(jobID, queued, time.Now(), false))
	rp.Admit()
	require.Len(t, rp.GetAllocationSummary(queued).Resources, 1)

	rp.AllocateRequest(restoreTestRequest(jobID, running, time.Now(), true))
	rp.Admit()
	require.Len(t, rp.GetAllocationSummary(running).Resources, 1)
}

// A restore whose reattach fails is retried on each admission pass until its allocation releases
// it. It holds no slots of its experiment meanwhile, so the next trial is admitted after it.
func TestRestoreFailedReattachHoldsNoSlots(t *testing.T) {
	failed, next := model.AllocationID(uuid.NewString()), model.AllocationID(uuid.NewString())
	rp, jobID := restorePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sub := rmevents.Subscribe(failed)
	defer sub.Close()

	rp.AllocateRequest(restoreTestRequest(jobID, failed, time.Now(), true))
	for i := 0; i < 2; i++ {
		// Any scheduling event makes the next admission pass retry the restore.
		rp.SetGroupMaxSlots(sproto.SetGroupMaxSlots{MaxSlots: ptrs.Ptr(1), JobID: jobID})
		rp.Admit()
		failure := poll[*sproto.ResourcesFailedError](ctx, t, sub)
		require.Equal(t, sproto.ResourcesMissing, failure.FailureType)
	}
	require.Empty(t, rp.GetAllocationSummary(failed).Resources)

	rp.ResourcesReleased(sproto.ResourcesReleased{AllocationID: failed})
	require.Zero(t, rp.slotsUsedPerGroup[rp.groups[jobID]])
	rp.AllocateRequest(restoreTestRequest(jobID, next, time.Now(), false))
	rp.Admit()
	require.Len(t, rp.GetAllocationSummary(next).Resources, 1)
}

// A request released before it was admitted leaves the slots of its experiment's running trial
// counted.
func TestReleaseQueuedKeepsRunningSlots(t *testing.T) {
	running, queued, next := model.AllocationID(uuid.NewString()),
		model.AllocationID(uuid.NewString()), model.AllocationID(uuid.NewString())
	rp, jobID := restorePool(t)
	rp.AllocateRequest(restoreTestRequest(jobID, running, time.Now().Add(-time.Second), false))
	rp.AllocateRequest(restoreTestRequest(jobID, queued, time.Now(), false))
	rp.Admit()
	require.Len(t, rp.GetAllocationSummary(running).Resources, 1)
	require.Empty(t, rp.GetAllocationSummary(queued).Resources)

	rp.ResourcesReleased(sproto.ResourcesReleased{AllocationID: queued})
	require.Equal(t, 1, rp.slotsUsedPerGroup[rp.groups[jobID]])
	rp.AllocateRequest(restoreTestRequest(jobID, next, time.Now(), false))
	rp.Admit()
	require.Empty(t, rp.GetAllocationSummary(next).Resources)
}
