//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

// lockJobPrivacyPolicy holds the job privacy policy for the test, which may change it, and puts
// the seeded public policy back when the test ends.
func lockJobPrivacyPolicy(t *testing.T) {
	unlock, err := db.LockJobPrivacyPolicyForTests(context.Background())
	require.NoError(t, err)
	t.Cleanup(unlock)
	t.Cleanup(func() {
		_, err := db.Bun().NewRaw(`INSERT INTO job_privacy_policy
			(kind, sharing_allowed, new_job_audience) VALUES
			('title', TRUE, '{authenticated}'), ('logs', TRUE, '{authenticated}'),
			('config', TRUE, '{authenticated}')
			ON CONFLICT (kind) DO UPDATE SET sharing_allowed = TRUE,
				new_job_audience = '{authenticated}', updated_by = NULL`).
			Exec(context.Background())
		require.NoError(t, err)
	})
}

// setJobPrivacyRule sets one policy row directly.
func setJobPrivacyRule(t *testing.T, kind string, allowed bool) {
	t.Helper()
	_, err := db.Bun().NewRaw(`UPDATE job_privacy_policy SET sharing_allowed = ? WHERE kind = ?`,
		allowed, kind).Exec(context.Background())
	require.NoError(t, err)
}

// requireStatus checks the code and message of a gRPC error.
func requireStatus(t *testing.T, err error, code codes.Code, msg string) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, status.Code(err), err.Error())
	if msg != "" {
		require.Equal(t, msg, status.Convert(err).Message())
	}
}

// jobPrivacyRule is a rule of the policy API.
func jobPrivacyRule(k jobv1.JobContentKind, allowed, allUsers bool) *jobv1.JobPrivacyRule {
	return &jobv1.JobPrivacyRule{
		Kind: k, SharingAllowed: allowed, NewJobAudience: &jobv1.JobAudience{AllUsers: allUsers},
	}
}

func TestJobPrivacyPolicyRPCs(t *testing.T) {
	api, admin, adminCtx := setupAPITest(t, nil)
	lockJobPrivacyPolicy(t)
	other := addGCTestOwner(t)
	otherCtx := userContext(t, other)
	const (
		title  = jobv1.JobContentKind_JOB_CONTENT_KIND_TITLE
		logs   = jobv1.JobContentKind_JOB_CONTENT_KIND_LOGS
		config = jobv1.JobContentKind_JOB_CONTENT_KIND_CONFIG
	)

	// Every user reads the policy, seeded public.
	got, err := api.GetJobPrivacyPolicy(otherCtx, &apiv1.GetJobPrivacyPolicyRequest{})
	require.NoError(t, err)
	require.Equal(t, []*jobv1.JobPrivacyRule{
		jobPrivacyRule(title, true, true), jobPrivacyRule(logs, true, true),
		jobPrivacyRule(config, true, true),
	}, got.Rules)

	want := []*jobv1.JobPrivacyRule{
		jobPrivacyRule(config, true, true), jobPrivacyRule(title, true, false),
		jobPrivacyRule(logs, false, false),
	}
	_, err = api.PutJobPrivacyPolicy(otherCtx, &apiv1.PutJobPrivacyPolicyRequest{Rules: want})
	requireStatus(t, err, codes.PermissionDenied, "")

	for name, rules := range map[string][]*jobv1.JobPrivacyRule{
		"missing kind": want[:2],
		"duplicate kind": append([]*jobv1.JobPrivacyRule{jobPrivacyRule(title, true, true)},
			want...),
		"unspecified kind": append([]*jobv1.JobPrivacyRule{jobPrivacyRule(
			jobv1.JobContentKind_JOB_CONTENT_KIND_UNSPECIFIED, true, true)}, want...),
		"unknown kind": append([]*jobv1.JobPrivacyRule{jobPrivacyRule(7, true, true)}, want...),
		"no audience":  {want[0], want[1], {Kind: logs}},
	} {
		_, err = api.PutJobPrivacyPolicy(adminCtx, &apiv1.PutJobPrivacyPolicyRequest{Rules: rules})
		requireStatus(t, err, codes.InvalidArgument, "")
		got, err = api.GetJobPrivacyPolicy(otherCtx, &apiv1.GetJobPrivacyPolicyRequest{})
		require.NoError(t, err)
		require.True(t, got.Rules[1].SharingAllowed, "%s changed the policy", name)
	}

	put, err := api.PutJobPrivacyPolicy(adminCtx, &apiv1.PutJobPrivacyPolicyRequest{Rules: want})
	require.NoError(t, err)
	stored := []*jobv1.JobPrivacyRule{want[1], want[2], want[0]}
	require.Equal(t, stored, put.Rules)
	got, err = api.GetJobPrivacyPolicy(otherCtx, &apiv1.GetJobPrivacyPolicyRequest{})
	require.NoError(t, err)
	require.Equal(t, stored, got.Rules)

	var updatedBy []int
	require.NoError(t, db.Bun().NewRaw(`SELECT updated_by FROM job_privacy_policy`).
		Scan(adminCtx, &updatedBy))
	require.Equal(t, []int{int(admin.ID), int(admin.ID), int(admin.ID)}, updatedBy)

	// A missing row forbids its kind; writing the policy brings it back.
	_, err = db.Bun().NewRaw(`DELETE FROM job_privacy_policy WHERE kind = 'config'`).Exec(adminCtx)
	require.NoError(t, err)
	got, err = api.GetJobPrivacyPolicy(otherCtx, &apiv1.GetJobPrivacyPolicyRequest{})
	require.NoError(t, err)
	require.Equal(t, jobPrivacyRule(config, false, false), got.Rules[2])
	put, err = api.PutJobPrivacyPolicy(adminCtx, &apiv1.PutJobPrivacyPolicyRequest{Rules: want})
	require.NoError(t, err)
	require.Equal(t, stored, put.Rules)
}

func TestMyJobPrivacyRPCs(t *testing.T) {
	api, _, _ := setupAPITest(t, nil)
	lockJobPrivacyPolicy(t)
	u := addGCTestOwner(t)
	ctx := userContext(t, u)

	got, err := api.GetMyJobPrivacy(ctx, &apiv1.GetMyJobPrivacyRequest{})
	require.NoError(t, err)
	require.Equal(t, &jobv1.UserJobPrivacy{ShareTitles: true, ShareLogs: true}, got.Privacy)

	patched, err := api.PatchMyJobPrivacy(ctx, &apiv1.PatchMyJobPrivacyRequest{
		ShareLogs:    ptrs.Ptr(false),
		NewJobTitles: &jobv1.JobAudience{AllUsers: false},
		NewJobLogs:   &jobv1.JobAudience{AllUsers: true},
	})
	require.NoError(t, err)
	want := &jobv1.UserJobPrivacy{
		ShareTitles:  true,
		ShareLogs:    false,
		NewJobTitles: &jobv1.JobAudience{AllUsers: false},
		NewJobLogs:   &jobv1.JobAudience{AllUsers: true},
	}
	require.Equal(t, want, patched.Privacy)
	got, err = api.GetMyJobPrivacy(ctx, &apiv1.GetMyJobPrivacyRequest{})
	require.NoError(t, err)
	require.Equal(t, want, got.Privacy)

	var row struct {
		ShareTitles bool     `bun:"share_titles"`
		ShareLogs   bool     `bun:"share_logs"`
		Titles      []string `bun:"new_job_title_audience,array"`
		Logs        []string `bun:"new_job_logs_audience,array"`
	}
	readRow := func() {
		t.Helper()
		require.NoError(t, db.Bun().NewRaw(`SELECT share_titles, share_logs,
			new_job_title_audience, new_job_logs_audience FROM user_job_privacy WHERE user_id = ?`,
			u.ID).Scan(ctx, &row))
	}
	readRow()
	require.False(t, row.ShareLogs)
	require.Equal(t, []string{}, row.Titles)
	require.Equal(t, []string{"authenticated"}, row.Logs)

	// A reset returns to the cluster default and keeps the other fields.
	patched, err = api.PatchMyJobPrivacy(ctx, &apiv1.PatchMyJobPrivacyRequest{
		ResetNewJobTitles: true,
	})
	require.NoError(t, err)
	want.NewJobTitles = nil
	require.Equal(t, want, patched.Privacy)
	readRow()
	require.Nil(t, row.Titles)
	require.Equal(t, []string{"authenticated"}, row.Logs)

	_, err = api.PatchMyJobPrivacy(ctx, &apiv1.PatchMyJobPrivacyRequest{
		NewJobLogs: &jobv1.JobAudience{}, ResetNewJobLogs: true,
	})
	requireStatus(t, err, codes.InvalidArgument, "")

	// Sharing new jobs' logs with all users is refused while the policy forbids it; owner only and
	// the switches are not.
	setJobPrivacyRule(t, "logs", false)
	_, err = api.PatchMyJobPrivacy(ctx, &apiv1.PatchMyJobPrivacyRequest{
		NewJobLogs: &jobv1.JobAudience{AllUsers: true},
	})
	requireStatus(t, err, codes.FailedPrecondition, "sharing of logs is forbidden")
	patched, err = api.PatchMyJobPrivacy(ctx, &apiv1.PatchMyJobPrivacyRequest{
		ShareLogs: ptrs.Ptr(true), NewJobLogs: &jobv1.JobAudience{AllUsers: false},
	})
	require.NoError(t, err)
	require.Equal(t, &jobv1.UserJobPrivacy{
		ShareTitles: true, ShareLogs: true, NewJobLogs: &jobv1.JobAudience{},
	}, patched.Privacy)
}

func TestJobSharingRPCs(t *testing.T) {
	api, admin, adminCtx := setupAPITest(t, nil)
	lockJobPrivacyPolicy(t)
	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	ownerCtx, otherCtx := userContext(t, owner), userContext(t, other)

	jobID := db.RequireMockJob(t, db.SingleDB(), &owner.ID)
	all := &jobv1.JobAudience{AllUsers: true}
	none := &jobv1.JobAudience{}

	unknown := model.NewJobID().String()
	for _, ctx := range []context.Context{adminCtx, otherCtx} {
		_, err := api.GetJobSharing(ctx, &apiv1.GetJobSharingRequest{JobId: unknown})
		requireStatus(t, err, codes.NotFound, "")
		_, err = api.PatchJobSharing(ctx, &apiv1.PatchJobSharingRequest{JobId: unknown, Title: all})
		requireStatus(t, err, codes.NotFound, "")
	}
	_, err := api.GetJobSharing(otherCtx, &apiv1.GetJobSharingRequest{JobId: jobID.String()})
	requireStatus(t, err, codes.PermissionDenied, "")
	_, err = api.PatchJobSharing(otherCtx, &apiv1.PatchJobSharingRequest{
		JobId: jobID.String(), Title: none,
	})
	requireStatus(t, err, codes.PermissionDenied, "")

	got, err := api.GetJobSharing(ownerCtx, &apiv1.GetJobSharingRequest{JobId: jobID.String()})
	require.NoError(t, err)
	require.Equal(t, &jobv1.JobSharing{Title: all, Logs: all, Config: all}, got.Sharing)
	require.Len(t, got.Rules, 3)

	patched, err := api.PatchJobSharing(ownerCtx, &apiv1.PatchJobSharingRequest{
		JobId: jobID.String(), Logs: none,
	})
	require.NoError(t, err)
	require.Equal(t, all, patched.Sharing.Title)
	require.Equal(t, none, patched.Sharing.Logs)
	require.Equal(t, all, patched.Sharing.Config)
	require.Equal(t, int32(owner.ID), patched.Sharing.GetUpdatedBy())
	require.NotNil(t, patched.Sharing.UpdatedAt)

	// An admin manages any job's sharing, and is recorded.
	patched, err = api.PatchJobSharing(adminCtx, &apiv1.PatchJobSharingRequest{
		JobId: jobID.String(), Config: none,
	})
	require.NoError(t, err)
	require.Equal(t, int32(admin.ID), patched.Sharing.GetUpdatedBy())
	stored := storedJobAudiences(adminCtx, t, jobID)
	require.Equal(t, []string{"authenticated"}, stored.TitleAudience)
	require.Equal(t, []string{}, stored.LogsAudience)
	require.Equal(t, []string{}, stored.ConfigAudience)
	require.Equal(t, admin.ID, *stored.SharingUpdatedBy)
	got, err = api.GetJobSharing(adminCtx, &apiv1.GetJobSharingRequest{JobId: jobID.String()})
	require.NoError(t, err)
	require.Equal(t, patched.Sharing, got.Sharing)

	// Logs shared with an unshared title are stored as sent.
	patched, err = api.PatchJobSharing(ownerCtx, &apiv1.PatchJobSharingRequest{
		JobId: jobID.String(), Title: none, Logs: all,
	})
	require.NoError(t, err)
	require.Equal(t, none, patched.Sharing.Title)
	require.Equal(t, all, patched.Sharing.Logs)

	// A kind the policy forbids is not shared with all users, and nothing is written.
	setJobPrivacyRule(t, "config", false)
	_, err = api.PatchJobSharing(ownerCtx, &apiv1.PatchJobSharingRequest{
		JobId: jobID.String(), Title: all, Config: all,
	})
	requireStatus(t, err, codes.FailedPrecondition, "sharing of config is forbidden")
	stored = storedJobAudiences(adminCtx, t, jobID)
	require.Equal(t, []string{}, stored.TitleAudience)
	_, err = api.PatchJobSharing(ownerCtx, &apiv1.PatchJobSharingRequest{
		JobId: jobID.String(), Config: none,
	})
	require.NoError(t, err)

	// A job from before job privacy has no setting until one is made, kind by kind.
	legacy := model.NewJobID()
	_, err = db.Bun().NewRaw(`INSERT INTO jobs (job_id, job_type, owner_id)
		VALUES (?, 'COMMAND', ?)`, legacy, owner.ID).Exec(adminCtx)
	require.NoError(t, err)
	got, err = api.GetJobSharing(ownerCtx, &apiv1.GetJobSharingRequest{JobId: legacy.String()})
	require.NoError(t, err)
	require.Equal(t, &jobv1.JobSharing{}, got.Sharing)
	patched, err = api.PatchJobSharing(ownerCtx, &apiv1.PatchJobSharingRequest{
		JobId: legacy.String(), Title: none,
	})
	require.NoError(t, err)
	require.Equal(t, none, patched.Sharing.Title)
	require.Nil(t, patched.Sharing.Logs)
	require.Nil(t, patched.Sharing.Config)

	// An empty patch leaves the job as it is.
	before := storedJobAudiences(adminCtx, t, legacy)
	patched, err = api.PatchJobSharing(adminCtx, &apiv1.PatchJobSharingRequest{
		JobId: legacy.String(),
	})
	require.NoError(t, err)
	require.Equal(t, int32(owner.ID), patched.Sharing.GetUpdatedBy())
	require.Equal(t, before, storedJobAudiences(adminCtx, t, legacy))
}

// TestJobPrivacyCapabilities checks the capabilities on every job-bearing response: a job whose
// logs its owner keeps to itself is readable in title and config, not in logs, by another user,
// and fully by its owner and admins; only they manage its sharing.
func TestJobPrivacyCapabilities(t *testing.T) {
	var aux, compute string
	var summaries map[model.AllocationID]sproto.AllocationSummary
	mockRM := mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute))
	mockRM.On("GetAllocationSummaries", mock.Anything).Return(
		func() map[model.AllocationID]sproto.AllocationSummary { return summaries }, nil)
	mockRM.On("GetJobQ", mock.Anything).Return(map[model.JobID]*sproto.RMJobInfo{}, nil)
	api, admin, adminCtx := setupAPITest(t, nil, mockRM)
	jobservice.SetDefaultService(mockRM)
	lockJobPrivacyPolicy(t)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)

	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	ownerCtx := userContext(t, owner)

	// An experiment with a trial, in a project of its own.
	projectID, _ := db.RequireMockProjectID(t, db.SingleDB(), 1, false)
	exp := createTestExpWithProjectID(t, api, owner, projectID)
	_, err = db.Bun().NewRaw(`UPDATE experiments SET state = 'COMPLETED' WHERE id = ?`, exp.ID).
		Exec(adminCtx)
	require.NoError(t, err)
	_, trialTask := db.RequireMockTrial(t, db.SingleDB(), exp)
	_, err = db.Bun().NewRaw(`UPDATE tasks SET job_id = ? WHERE task_id = ?`,
		exp.JobID, trialTask.TaskID).Exec(adminCtx)
	require.NoError(t, err)

	cmd, err := api.LaunchCommand(ownerCtx, &apiv1.LaunchCommandRequest{})
	require.NoError(t, err)
	shell, err := api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{})
	require.NoError(t, err)
	// Generic tasks start their allocations in a capturing service; commands and shells keep theirs.
	oldService := task.DefaultService
	task.DefaultService = &captureAllocationService{AllocationService: oldService}
	t.Cleanup(func() { task.DefaultService = oldService })
	generic, err := createGenericTaskForAccessTest(ownerCtx, t, api,
		&apiv1.CreateGenericTaskRequest{Config: "entrypoint: [\"true\"]\n"})
	require.NoError(t, err)
	genericJob := taskJobID(adminCtx, t, generic.TaskId)

	for _, id := range []string{
		exp.JobID.String(), cmd.Command.JobId, shell.Shell.JobId, genericJob.String(),
	} {
		_, err = api.PatchJobSharing(ownerCtx, &apiv1.PatchJobSharingRequest{
			JobId: id, Logs: &jobv1.JobAudience{},
		})
		require.NoError(t, err)
	}
	mockRM.On("GetExternalJobs", mock.Anything).Return(func(rm.ResourcePoolName) []*jobv1.Job {
		return []*jobv1.Job{{JobId: exp.JobID.String(), SubmissionTime: timestamppb.Now()}}
	}, nil)
	summaries = map[model.AllocationID]sproto.AllocationSummary{
		model.AllocationID(fmt.Sprintf("%s.1", trialTask.TaskID)): {
			TaskID: trialTask.TaskID, AllocationID: model.AllocationID(trialTask.TaskID + ".1"),
			RegisteredTime: time.Now(), ResourcePool: "default",
		},
	}

	full := &jobv1.JobCapabilities{
		ViewTitle: true, ViewLogs: true, ViewConfig: true, ManageSharing: true,
	}
	for name, tc := range map[string]struct {
		viewer model.User
		want   *jobv1.JobCapabilities
	}{
		"owner": {owner, full},
		"admin": {admin, full},
		"other": {other, &jobv1.JobCapabilities{ViewTitle: true, ViewConfig: true}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, want := userContext(t, tc.viewer), tc.want
			getExp, err := api.GetExperiment(ctx, &apiv1.GetExperimentRequest{
				ExperimentId: int32(exp.ID),
			})
			require.NoError(t, err)
			require.Equal(t, want, getExp.Experiment.Capabilities, "GetExperiment")

			exps, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{
				ProjectId: int32(projectID),
			})
			require.NoError(t, err)
			require.Len(t, exps.Experiments, 1)
			require.Equal(t, want, exps.Experiments[0].Capabilities, "GetExperiments")

			search, err := api.SearchExperiments(ctx, &apiv1.SearchExperimentsRequest{
				ProjectId: ptrs.Ptr(int32(projectID)),
			})
			require.NoError(t, err)
			require.Len(t, search.Experiments, 1)
			require.Equal(t, want, search.Experiments[0].Experiment.Capabilities,
				"SearchExperiments")

			runs, err := api.SearchRuns(ctx, &apiv1.SearchRunsRequest{
				ProjectId: ptrs.Ptr(int32(projectID)),
			})
			require.NoError(t, err)
			require.Len(t, runs.Runs, 1)
			require.Equal(t, want, runs.Runs[0].Experiment.Capabilities, "SearchRuns")

			cmds, err := api.GetCommands(ctx, &apiv1.GetCommandsRequest{})
			require.NoError(t, err)
			found := false
			for _, c := range cmds.Commands {
				if c.Id == cmd.Command.Id {
					found = true
					require.Equal(t, want, c.Capabilities, "GetCommands")
				}
			}
			require.True(t, found)
			getCmd, err := api.GetCommand(ctx, &apiv1.GetCommandRequest{CommandId: cmd.Command.Id})
			require.NoError(t, err)
			require.Equal(t, want, getCmd.Command.Capabilities, "GetCommand")

			shells, err := api.GetShells(ctx, &apiv1.GetShellsRequest{})
			require.NoError(t, err)
			found = false
			for _, s := range shells.Shells {
				if s.Id == shell.Shell.Id {
					found = true
					require.Equal(t, want, s.Capabilities, "GetShells")
				}
			}
			require.True(t, found)
			getShell, err := api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: shell.Shell.Id})
			require.NoError(t, err)
			require.Equal(t, want, getShell.Shell.Capabilities, "GetShell")

			generics, err := api.GetGenericTasks(ctx, &apiv1.GetGenericTasksRequest{
				TaskIds: []string{generic.TaskId},
			})
			require.NoError(t, err)
			require.Len(t, generics.Tasks, 1)
			require.Equal(t, want, generics.Tasks[0].Capabilities, "GetGenericTasks")

			jobs, err := api.GetJobs(ctx, &apiv1.GetJobsRequest{ResourcePool: "default"})
			require.NoError(t, err)
			require.Len(t, jobs.Jobs, 1)
			require.Equal(t, want, jobs.Jobs[0].Capabilities, "GetJobs")
			jobsV2, err := api.GetJobsV2(ctx, &apiv1.GetJobsV2Request{ResourcePool: "default"})
			require.NoError(t, err)
			require.Len(t, jobsV2.Jobs, 1)
			require.Equal(t, want, jobsV2.Jobs[0].GetFull().Capabilities, "GetJobsV2")

			tasks, err := api.GetTasks(ctx, &apiv1.GetTasksRequest{})
			require.NoError(t, err)
			require.Len(t, tasks.AllocationIdToSummary, 1)
			for _, s := range tasks.AllocationIdToSummary {
				require.Equal(t, want, s.Capabilities, "GetTasks")
			}
		})
	}
}
