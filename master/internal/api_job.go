package internal

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/api/apiutils"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/internal/poolaccess"
	"github.com/determined-ai/determined/master/internal/rm"

	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/job"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

// GetJobs retrieves a list of jobs for a resource pool.
func (a *apiServer) GetJobs(
	ctx context.Context, req *apiv1.GetJobsRequest,
) (resp *apiv1.GetJobsResponse, err error) {
	jobs, err := jobservice.DefaultService.GetJobs(
		rm.ResourcePoolName(req.ResourcePool),
		req.OrderBy == apiv1.OrderBy_ORDER_BY_DESC,
		req.States,
	)
	if err != nil {
		return nil, err
	}
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}
	resp = &apiv1.GetJobsResponse{Jobs: make([]*jobv1.Job, 0)}

	jobs, err = job.AuthZProvider.Get().FilterJobs(ctx, *curUser, jobs)
	if err != nil {
		return nil, err
	}
	if err = setQueueJobCapabilities(ctx, *curUser, jobs); err != nil {
		return nil, err
	}
	resp.Jobs = jobs

	if req.Limit == 0 {
		req.Limit = 100
	}

	if err = api.Paginate(&resp.Pagination, &resp.Jobs, req.Offset, req.Limit); err != nil {
		return nil, err
	}
	if err = a.enrichJobAllocationStartTimes(ctx, resp.Jobs); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetJobsV2 retrieves a list of jobs for a resource pool.
func (a *apiServer) GetJobsV2(
	ctx context.Context, req *apiv1.GetJobsV2Request,
) (resp *apiv1.GetJobsV2Response, err error) {
	jobs, err := jobservice.DefaultService.GetJobs(
		rm.ResourcePoolName(req.ResourcePool),
		req.OrderBy == apiv1.OrderBy_ORDER_BY_DESC,
		req.States,
	)
	if err != nil {
		return nil, err
	}
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}
	resp = &apiv1.GetJobsV2Response{Jobs: make([]*jobv1.RBACJob, 0)}

	okJobs, err := job.AuthZProvider.Get().FilterJobs(ctx, *curUser, jobs)
	if err != nil {
		return nil, err
	}
	// Limited jobs carry no content, so they get no capabilities.
	if err = setQueueJobCapabilities(ctx, *curUser, okJobs); err != nil {
		return nil, err
	}
	okJobsMap := make(map[string]bool)
	for _, j := range okJobs {
		okJobsMap[j.JobId] = true
	}

	for _, job := range jobs {
		j := jobv1.RBACJob{}
		if ok := okJobsMap[job.JobId]; ok {
			j.Job = &jobv1.RBACJob_Full{
				Full: job,
			}
		} else {
			limitedJob := authz.ObfuscateJob(job)
			j.Job = &jobv1.RBACJob_Limited{
				Limited: &limitedJob,
			}
		}
		resp.Jobs = append(resp.Jobs, &j)
	}

	if req.Limit == 0 {
		req.Limit = 100
	}

	if err = api.Paginate(&resp.Pagination, &resp.Jobs, req.Offset, req.Limit); err != nil {
		return nil, err
	}
	// Limited jobs retain their omitted metadata, including the assignment timestamp.
	fullJobs := make([]*jobv1.Job, 0, len(resp.Jobs))
	for _, item := range resp.Jobs {
		if full := item.GetFull(); full != nil {
			fullJobs = append(fullJobs, full)
		}
	}
	if err = a.enrichJobAllocationStartTimes(ctx, fullJobs); err != nil {
		return nil, err
	}
	return resp, nil
}

// setQueueJobCapabilities sets the user's job privacy capabilities on the queue's jobs and clears
// the name of a job whose title the user may not read.
func setQueueJobCapabilities(ctx context.Context, curUser model.User, jobs []*jobv1.Job) error {
	v, err := jobprivacy.NewViewer(ctx, curUser)
	if err != nil {
		return err
	}
	return applyJobPrivacy(ctx, v, jobs,
		func(j *jobv1.Job) string { return j.JobId },
		func(j *jobv1.Job, c jobprivacy.Caps) {
			if !c.ViewTitle {
				j.Name = ""
			}
			j.Capabilities = c.Proto()
		})
}

func (a *apiServer) enrichJobAllocationStartTimes(ctx context.Context, jobs []*jobv1.Job) error {
	starts := make([]allocationStartTarget, 0, len(jobs))
	for _, item := range jobs {
		starts = append(starts, allocationStartTarget{item.JobId, &item.AllocationStartTime})
	}
	return a.enrichAllocationStartTimes(ctx, starts, allocationStartJob)
}

// GetJobQueueStats retrieves job queue stats for a set of resource pools.
func (a *apiServer) GetJobQueueStats(
	_ context.Context, req *apiv1.GetJobQueueStatsRequest,
) (*apiv1.GetJobQueueStatsResponse, error) {
	resp, err := a.m.rm.GetJobQueueStatsRequest(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// UpdateJobQueue forwards the job queue message to the relevant resource pool.
func (a *apiServer) UpdateJobQueue(
	ctx context.Context, req *apiv1.UpdateJobQueueRequest,
) (*apiv1.UpdateJobQueueResponse, error) {
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}
	permErr, err := job.AuthZProvider.Get().CanControlJobQueue(ctx, curUser)
	if err != nil {
		return nil, err
	}
	if permErr != nil {
		return nil, permErr
	}
	// Preflight the whole batch so an unauthorized target cannot be changed by a
	// priority, weight, or resource-pool update, even when another target is owned.
	err = updateJobQueueAuthorized(ctx, *curUser, req.Updates,
		job.AuthZProvider.Get().CanControlJobQueueUpdate,
		jobservice.DefaultService.UpdateJobQueue)
	if err != nil {
		return nil, err
	}
	return &apiv1.UpdateJobQueueResponse{}, nil
}

func updateJobQueueAuthorized(
	ctx context.Context, curUser model.User, updates []*jobv1.QueueControl,
	authorize func(context.Context, model.User, model.JobID) error,
	apply func([]*jobv1.QueueControl) error,
) error {
	for _, update := range updates {
		if update == nil || update.JobId == "" {
			return apiutils.MapAndFilterErrors(db.ErrInvalidInput, nil, nil)
		}
		if err := authorize(ctx, curUser, model.JobID(update.JobId)); err != nil {
			return apiutils.MapAndFilterErrors(err, nil, nil)
		}
		// Moving a job to another pool is a new admission into the target pool. Priority and
		// weight changes manage accepted work and are not checked.
		if move, ok := update.Action.(*jobv1.QueueControl_ResourcePool); ok {
			if move.ResourcePool == "" {
				// An empty target would resolve to a default pool: an unchecked re-route.
				return status.Error(codes.InvalidArgument,
					"moving a job to another resource pool requires the target pool name")
			}
			if err := poolaccess.CanUseResourcePool(ctx, curUser, move.ResourcePool); err != nil {
				return err
			}
		}
	}
	return apply(updates)
}
