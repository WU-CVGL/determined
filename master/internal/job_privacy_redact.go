package internal

import (
	"context"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/checkpointv1"
	"github.com/determined-ai/determined/proto/pkg/experimentv1"
	"github.com/determined-ai/determined/proto/pkg/masterv1"
	"github.com/determined-ai/determined/proto/pkg/modelv1"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
	"github.com/determined-ai/determined/proto/pkg/trialv1"
)

// redactExperimentForViewer omits what the user may not read of an experiment that a handler
// returns, and sets the user's capabilities on it. A hidden title omits the name, description,
// labels and notes; a hidden config omits the config, the original config and the Pachyderm
// integration, and the response's copy of the config when respConfig is given. Internal callers
// read the experiment in full; only the response is redacted.
func redactExperimentForViewer(
	ctx context.Context, curUser model.User, exp *experimentv1.Experiment,
	respConfig **structpb.Struct,
) error {
	v, err := jobprivacy.NewViewer(ctx, curUser)
	if err != nil {
		return err
	}
	jobID := model.JobID(exp.JobId)
	caps, err := jobprivacy.ForJobs(ctx, v, []model.JobID{jobID})
	if err != nil {
		return err
	}
	c := caps[jobID]
	if !c.ViewTitle {
		exp.Name, exp.Description, exp.Labels, exp.Notes = "", "", nil, ""
	}
	if !c.ViewConfig {
		exp.Config, exp.OriginalConfig, exp.PachydermIntegration = nil, "", nil //nolint:staticcheck
		if respConfig != nil {
			*respConfig = nil
		}
	}
	exp.Capabilities = c.Proto()
	return nil
}

// labelsForViewer returns the labels of an experiment that an editor changed, or none when the
// user may not read its title.
func labelsForViewer(
	ctx context.Context, curUser model.User, exp *experimentv1.Experiment,
) ([]string, error) {
	v, err := jobprivacy.NewViewer(ctx, curUser)
	if err != nil {
		return nil, err
	}
	jobID := model.JobID(exp.JobId)
	caps, err := jobprivacy.ForJobs(ctx, v, []model.JobID{jobID})
	if err != nil {
		return nil, err
	}
	if !caps[jobID].ViewTitle {
		return nil, nil
	}
	return exp.Labels, nil
}

// experimentJobs returns the job of each experiment. An experiment that is not found is not in
// the result. A query error is Unavailable, as the other job privacy reads.
func experimentJobs(ctx context.Context, expIDs []int32) (map[int32]model.JobID, error) {
	jobs := make(map[int32]model.JobID, len(expIDs))
	if len(expIDs) == 0 {
		return jobs, nil
	}
	var rows []struct {
		ID    int32       `bun:"id"`
		JobID model.JobID `bun:"job_id"`
	}
	if err := db.Bun().NewSelect().Table("experiments").Column("id", "job_id").
		Where("id IN (?)", bun.In(expIDs)).Scan(ctx, &rows); err != nil {
		return nil, status.Errorf(codes.Unavailable, "could not read job privacy: %s; try again", err)
	}
	for _, r := range rows {
		jobs[r.ID] = r.JobID
	}
	return jobs, nil
}

// experimentCaps returns the viewer's capabilities on the job of each experiment. An experiment
// that is not found gets none for a non-admin.
func experimentCaps(
	ctx context.Context, v jobprivacy.Viewer, expIDs []int32,
) (map[int32]jobprivacy.Caps, error) {
	result := make(map[int32]jobprivacy.Caps, len(expIDs))
	if v.Admin {
		for _, id := range expIDs {
			result[id] = jobprivacy.AllCaps
		}
		return result, nil
	}
	jobs, err := experimentJobs(ctx, expIDs)
	if err != nil {
		return nil, err
	}
	jobIDs := make([]model.JobID, 0, len(jobs))
	for _, j := range jobs {
		jobIDs = append(jobIDs, j)
	}
	caps, err := jobprivacy.ForJobs(ctx, v, jobIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range expIDs {
		if j, ok := jobs[id]; ok {
			result[id] = caps[j]
		} else {
			result[id] = jobprivacy.Caps{}
		}
	}
	return result, nil
}

// redactCheckpointTraining reduces the experiment config of each checkpoint whose experiment's
// config the user may not read to its checkpoint storage, which is what the SDK needs to download
// the checkpoint. A checkpoint with a config but no experiment is reduced for non-admins. The
// hyperparameters stay.
func redactCheckpointTraining(
	ctx context.Context, curUser model.User, ckpts ...*checkpointv1.Checkpoint,
) error {
	var expIDs []int32
	for _, c := range ckpts {
		if c.GetTraining().GetExperimentConfig() != nil {
			expIDs = append(expIDs, c.Training.GetExperimentId())
		}
	}
	if len(expIDs) == 0 {
		return nil
	}
	v, err := jobprivacy.NewViewer(ctx, curUser)
	if err != nil {
		return err
	}
	if v.Admin {
		return nil
	}
	caps, err := experimentCaps(ctx, v, expIDs)
	if err != nil {
		return err
	}
	for _, c := range ckpts {
		cfg := c.GetTraining().GetExperimentConfig()
		if cfg == nil || caps[c.Training.GetExperimentId()].ViewConfig {
			continue
		}
		reduced := &structpb.Struct{Fields: map[string]*structpb.Value{}}
		if storage, ok := cfg.Fields["checkpoint_storage"]; ok {
			reduced.Fields["checkpoint_storage"] = storage
		}
		c.Training.ExperimentConfig = reduced
	}
	return nil
}

// redactModelVersionCheckpoints is redactCheckpointTraining for the checkpoints of model versions.
func redactModelVersionCheckpoints(
	ctx context.Context, curUser model.User, mvs ...*modelv1.ModelVersion,
) error {
	ckpts := make([]*checkpointv1.Checkpoint, 0, len(mvs))
	for _, mv := range mvs {
		if mv.GetCheckpoint() != nil {
			ckpts = append(ckpts, mv.Checkpoint)
		}
	}
	return redactCheckpointTraining(ctx, curUser, ckpts...)
}

// redactTrialsLogPolicy clears the log policy match of each trial whose experiment's logs the user
// may not read: the match is made from the logs.
func redactTrialsLogPolicy(
	ctx context.Context, curUser model.User, trials ...*trialv1.Trial,
) error {
	var expIDs []int32
	for _, t := range trials {
		if t.LogPolicyMatched != nil {
			expIDs = append(expIDs, t.ExperimentId)
		}
	}
	if len(expIDs) == 0 {
		return nil
	}
	v, err := jobprivacy.NewViewer(ctx, curUser)
	if err != nil {
		return err
	}
	caps, err := experimentCaps(ctx, v, expIDs)
	if err != nil {
		return err
	}
	for _, t := range trials {
		if t.LogPolicyMatched != nil && !caps[t.ExperimentId].ViewLogs {
			t.LogPolicyMatched = nil
		}
	}
	return nil
}

// redactRawAllocationLabels clears the labels of each raw usage entry whose experiment's title
// the user may not read.
func redactRawAllocationLabels(
	ctx context.Context, curUser model.User, entries []*masterv1.ResourceAllocationRawEntry,
) error {
	var expIDs []int32
	for _, e := range entries {
		if len(e.Labels) > 0 {
			expIDs = append(expIDs, e.ExperimentId)
		}
	}
	if len(expIDs) == 0 {
		return nil
	}
	v, err := jobprivacy.NewViewer(ctx, curUser)
	if err != nil {
		return err
	}
	caps, err := experimentCaps(ctx, v, expIDs)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if len(e.Labels) > 0 && !caps[e.ExperimentId].ViewTitle {
			e.Labels = nil
		}
	}
	return nil
}

// taskCaps returns the viewer's capabilities on a task, resolved by jobprivacy.TaskJobs as
// jobprivacy.ForTasks and canReadTaskLogs do. An unknown task is NotFound for a non-admin.
func taskCaps(
	ctx context.Context, v jobprivacy.Viewer, taskID model.TaskID,
) (jobprivacy.Caps, error) {
	if v.Admin {
		return jobprivacy.AllCaps, nil
	}
	tasks, err := jobprivacy.TaskJobs(ctx, nil, []model.TaskID{taskID})
	if err != nil {
		return jobprivacy.Caps{}, err
	}
	if _, ok := tasks[taskID]; !ok {
		return jobprivacy.Caps{}, api.NotFoundErrs("task", fmt.Sprint(taskID), true)
	}
	caps, err := jobprivacy.ForTaskJobs(ctx, v, tasks)
	if err != nil {
		return jobprivacy.Caps{}, err
	}
	return caps[taskID], nil
}

// The free-text failure reasons of tasks, allocations, commands and shells carry startup
// diagnostics that are also written to the task log, so they follow the logs: for a user who may
// not read the logs they are empty. States, failure types, exit codes, resources and times stay.

// redactTask clears the exit reason of each of the task's allocations unless caps allow the logs.
func redactTask(t *taskv1.Task, caps jobprivacy.Caps) {
	if caps.ViewLogs {
		return
	}
	for _, a := range t.Allocations {
		redactAllocation(a, caps)
	}
}

// redactAllocation clears the allocation's exit reason unless caps allow the logs.
func redactAllocation(a *taskv1.Allocation, caps jobprivacy.Caps) {
	if !caps.ViewLogs && a.ExitReason != nil {
		a.ExitReason = ptrs.Ptr("")
	}
}

// redactAllocationSummary clears the summary's name unless caps allow the title, and the error
// messages of its resources' failures unless caps allow the logs.
func redactAllocationSummary(s *taskv1.AllocationSummary, caps jobprivacy.Caps) {
	if !caps.ViewTitle {
		s.Name = ""
	}
	if caps.ViewLogs {
		return
	}
	for _, r := range s.Resources {
		if f := r.GetExited().GetFailure(); f != nil {
			f.ErrMsg = ""
		}
	}
}

// redactAllocationSummaryCopy is redactAllocationSummary for the resource manager's summary, which
// the HTTP route returns as it is. It returns a redacted copy and never writes through the
// summary's pointers, which the resource manager shares.
func redactAllocationSummaryCopy(
	s sproto.AllocationSummary, caps jobprivacy.Caps,
) sproto.AllocationSummary {
	if !caps.ViewTitle {
		s.Name = ""
	}
	if caps.ViewLogs || len(s.Resources) == 0 {
		return s
	}
	resources := make([]sproto.ResourcesSummary, len(s.Resources))
	for i, r := range s.Resources {
		if r.Exited != nil && r.Exited.Failure != nil {
			failure := *r.Exited.Failure
			failure.ErrMsg = ""
			r.Exited = &sproto.ResourcesStopped{Failure: &failure}
		}
		resources[i] = r
	}
	s.Resources = resources
	return s
}

// echoJobPrivacyError answers a job privacy error on an HTTP route with the status its gRPC code
// has on the gRPC routes: Unavailable is 503. Any other error is returned as it is.
func echoJobPrivacyError(err error) error {
	if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
		return echo.NewHTTPError(http.StatusServiceUnavailable, s.Message())
	}
	return err
}
