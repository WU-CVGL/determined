//go:build integration
// +build integration

package kubernetesrm

import (
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
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

// restorePool returns a resource pool whose cluster has the job of the allocation running, and
// the ID of a job with max_slots 1.
func restorePool(t *testing.T, running model.AllocationID) (*kubernetesResourcePool, model.JobID) {
	const namespace = "restored"
	jobs := &mocks.JobInterface{}
	jobs.On("List", mock.Anything, mock.Anything).Return(&batchV1.JobList{Items: []batchV1.Job{{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      string(running),
			Namespace: namespace,
			Labels: map[string]string{
				determinedLabel:   string(running),
				resourcePoolLabel: defaultResourcePool,
			},
		},
	}}}, nil)
	configMaps := &mocks.ConfigMapInterface{}
	configMaps.On("List", mock.Anything, mock.Anything).Return(&k8sV1.ConfigMapList{
		Items: []k8sV1.ConfigMap{{ObjectMeta: metaV1.ObjectMeta{Name: string(running), Namespace: namespace}}},
	}, nil)
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
