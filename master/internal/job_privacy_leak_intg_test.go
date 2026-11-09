//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/checkpointv1"
	"github.com/determined-ai/determined/proto/pkg/commonv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
	"github.com/determined-ai/determined/proto/pkg/masterv1"
	"github.com/determined-ai/determined/proto/pkg/modelv1"
)

// The titles of the hidden tasks of the list tests.
const (
	secretJobName            = "secret-job"
	secretCommandDescription = "secret-command"
)

// setExperimentTitle sets an experiment's name, description and labels in its config, and notes.
func setExperimentTitle(
	ctx context.Context, t *testing.T, expID int, name, description string, labels ...string,
) {
	t.Helper()
	if labels == nil {
		labels = []string{}
	}
	labelsJSON, err := json.Marshal(labels)
	require.NoError(t, err)
	_, err = db.Bun().NewRaw(`UPDATE experiments SET
		config = config || jsonb_build_object('name', ?::text, 'description', ?::text,
			'labels', ?::jsonb),
		notes = ?
		WHERE id = ?`, name, description, string(labelsJSON), "notes of "+name, expID).Exec(ctx)
	require.NoError(t, err)
}

// shareJob sets a job's own sharing: each kind is shared with all users or kept to the owner.
func shareJob(
	ctx context.Context, t *testing.T, api *apiServer, jobID string, title, logs, config bool,
) {
	t.Helper()
	_, err := api.PatchJobSharing(ctx, &apiv1.PatchJobSharingRequest{
		JobId:  jobID,
		Title:  &jobv1.JobAudience{AllUsers: title},
		Logs:   &jobv1.JobAudience{AllUsers: logs},
		Config: &jobv1.JobAudience{AllUsers: config},
	})
	require.NoError(t, err)
}

// structKeys returns the keys of a struct, sorted.
func structKeys(s *structpb.Struct) []string {
	var keys []string
	for k := range s.GetFields() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// requireResourcesOnly checks that a list's config of an experiment whose config is hidden is the
// projection of its resources: the pool and slots, which are never hidden, and nothing else.
func requireResourcesOnly(t *testing.T, config *structpb.Struct, msg string) {
	t.Helper()
	require.Equal(t, []string{"resources"}, structKeys(config), msg)
	resources := config.Fields["resources"].GetStructValue()
	require.Equal(t, "kubernetes", resources.Fields["resource_pool"].GetStringValue(), msg)
	for _, k := range structKeys(resources) {
		require.Contains(t, []string{"resource_pool", "slots_per_trial"}, k, msg)
	}
}

// TestJobPrivacyExperimentReadPaths checks every experiment and run read path against an owner's
// experiments whose title, config or logs another user may not read: that user gets the content
// omitted, finds hidden experiments only by ID and sees them sort last whatever their names, while
// the owner and an admin get everything.
func TestJobPrivacyExperimentReadPaths(t *testing.T) {
	api, admin, adminCtx := setupAPITest(t, nil)
	lockJobPrivacyPolicy(t)
	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	ownerCtx, otherCtx := userContext(t, owner), userContext(t, other)
	projectID, _ := db.RequireMockProjectID(t, db.SingleDB(), 1, false)

	newExp := func(name, description string, labels ...string) *model.Experiment {
		exp := createTestExpWithProjectID(t, api, owner, projectID)
		setExperimentTitle(adminCtx, t, exp.ID, name, description, labels...)
		db.RequireMockTrial(t, db.SingleDB(), exp)
		return exp
	}
	hiddenA := newExp("a-secret", "secret-desc", "secret-label")
	visible := newExp("m-shared", "shared-desc", "shared-label")
	hiddenZ := newExp("z-secret", "secret-desc", "secret-label")
	configHidden := newExp("c-shared", "shared-desc", "shared-label")
	logsHidden := newExp("l-shared", "shared-desc", "shared-label")
	shareJob(ownerCtx, t, api, hiddenA.JobID.String(), false, false, false)
	shareJob(ownerCtx, t, api, hiddenZ.JobID.String(), false, false, false)
	shareJob(ownerCtx, t, api, configHidden.JobID.String(), true, true, false)
	shareJob(ownerCtx, t, api, logsHidden.JobID.String(), true, false, true)
	_, err := db.Bun().NewRaw(`UPDATE runs SET log_policy_matched = 'secret-policy'
		WHERE experiment_id = ?`, logsHidden.ID).Exec(adminCtx)
	require.NoError(t, err)
	hidden := []int32{int32(hiddenA.ID), int32(hiddenZ.ID)}

	full := jobprivacy.AllCaps.Proto()
	none := &jobv1.JobCapabilities{}
	viewers := map[string]struct {
		user model.User
		full bool
	}{
		"owner": {owner, true},
		"admin": {admin, true},
		"other": {other, false},
	}
	for name, vw := range viewers {
		t.Run(name, func(t *testing.T) {
			ctx := userContext(t, vw.user)

			// The experiment by ID.
			for _, exp := range []*model.Experiment{hiddenA, configHidden, logsHidden, visible} {
				got, err := api.GetExperiment(ctx, &apiv1.GetExperimentRequest{
					ExperimentId: int32(exp.ID),
				})
				require.NoError(t, err)
				e := got.Experiment
				require.Equal(t, int32(projectID), e.ProjectId)
				require.Equal(t, int32(owner.ID), e.UserId)
				require.Equal(t, "kubernetes", e.ResourcePool)
				require.Equal(t, exp.JobID.String(), e.JobId)
				titleOK := vw.full || exp.ID != hiddenA.ID
				configOK := titleOK && (vw.full || exp.ID != configHidden.ID)
				if titleOK {
					require.NotEmpty(t, e.Name)
					require.Equal(t, "notes of "+e.Name, e.Notes)
					require.NotEmpty(t, e.Labels)
				} else {
					require.Empty(t, e.Name)
					require.Empty(t, e.Description)
					require.Empty(t, e.Labels)
					require.Empty(t, e.Notes)
					require.Equal(t, none, e.Capabilities)
				}
				if configOK {
					require.NotNil(t, e.Config)   //nolint:staticcheck
					require.NotNil(t, got.Config) //nolint:staticcheck
				} else {
					require.Nil(t, e.Config)   //nolint:staticcheck
					require.Nil(t, got.Config) //nolint:staticcheck
					require.Empty(t, e.OriginalConfig)
					require.Nil(t, e.PachydermIntegration)
				}
				if vw.full {
					require.Equal(t, full, e.Capabilities)
				}
			}

			// The project's list.
			list, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{
				ProjectId: int32(projectID),
			})
			require.NoError(t, err)
			require.Len(t, list.Experiments, 5)
			for _, e := range list.Experiments {
				require.Equal(t, int32(owner.ID), e.UserId)
				require.Equal(t, "kubernetes", e.ResourcePool)
				switch {
				case vw.full:
					require.NotEmpty(t, e.Name)
					require.Equal(t, "omitted", e.Notes)
					require.Contains(t, structKeys(e.Config), "searcher") //nolint:staticcheck
					require.Equal(t, full, e.Capabilities)
				case slices.Contains(hidden, e.Id):
					require.Empty(t, e.Name)
					require.Empty(t, e.Description)
					require.Empty(t, e.Labels)
					require.Empty(t, e.Notes)
					requireResourcesOnly(t, e.Config, "hidden") //nolint:staticcheck
					require.Equal(t, none, e.Capabilities)
				case e.Id == int32(configHidden.ID):
					require.Equal(t, "c-shared", e.Name)
					requireResourcesOnly(t, e.Config, "config hidden") //nolint:staticcheck
					require.Equal(t,
						&jobv1.JobCapabilities{ViewTitle: true, ViewLogs: true}, e.Capabilities)
				default:
					require.NotEmpty(t, e.Name)
					require.Contains(t, structKeys(e.Config), "searcher") //nolint:staticcheck
				}
			}

			// Searches match a hidden title only by ID.
			wantHidden := 0
			if vw.full {
				wantHidden = 2
			}
			for _, req := range []*apiv1.GetExperimentsRequest{
				{ProjectId: int32(projectID), Name: "secret"},
				{ProjectId: int32(projectID), Description: "secret-desc"},
				{ProjectId: int32(projectID), Labels: []string{"secret-label"}},
			} {
				found, err := api.GetExperiments(ctx, req)
				require.NoError(t, err)
				require.Len(t, found.Experiments, wantHidden, "%v", req)
			}
			byID, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{
				ProjectId:          int32(projectID),
				ExperimentIdFilter: &commonv1.Int32FieldFilter{Incl: []int32{int32(hiddenA.ID)}},
			})
			require.NoError(t, err)
			require.Len(t, byID.Experiments, 1)

			for _, filter := range []string{
				`{"filterGroup":{"children":[{"columnName":"name","kind":"field",` +
					`"location":"LOCATION_TYPE_EXPERIMENT","operator":"contains","value":"secret"}],` +
					`"conjunction":"and","kind":"group"},"showArchived":true}`,
				`{"filterGroup":{"children":[{"columnName":"tags","kind":"field",` +
					`"location":"LOCATION_TYPE_EXPERIMENT","operator":"contains",` +
					`"value":"secret-label"}],"conjunction":"and","kind":"group"},"showArchived":true}`,
				`{"filterGroup":{"children":[{"columnName":"description","kind":"field",` +
					`"location":"LOCATION_TYPE_EXPERIMENT","operator":"=","value":"secret-desc"}],` +
					`"conjunction":"and","kind":"group"},"showArchived":true}`,
			} {
				found, err := api.SearchExperiments(ctx, &apiv1.SearchExperimentsRequest{
					ProjectId: ptrs.Ptr(int32(projectID)), Filter: ptrs.Ptr(filter),
				})
				require.NoError(t, err)
				require.Len(t, found.Experiments, wantHidden, filter)
			}
			for _, filter := range []string{
				`{"filterGroup":{"children":[{"columnName":"experimentName","kind":"field",` +
					`"location":"LOCATION_TYPE_RUN","operator":"contains","value":"secret"}],` +
					`"conjunction":"and","kind":"group"},"showArchived":true}`,
				`{"filterGroup":{"children":[{"columnName":"tags","kind":"field",` +
					`"location":"LOCATION_TYPE_RUN","operator":"contains","value":"secret-label"}],` +
					`"conjunction":"and","kind":"group"},"showArchived":true}`,
			} {
				found, err := api.SearchRuns(ctx, &apiv1.SearchRunsRequest{
					ProjectId: ptrs.Ptr(int32(projectID)), Filter: ptrs.Ptr(filter),
				})
				require.NoError(t, err)
				require.Len(t, found.Runs, wantHidden, filter)
			}
			ids, err := experiment.FilterToExperimentIds(ctx, vw.user, &apiv1.BulkExperimentFilters{
				ProjectId: int32(projectID), Name: "secret",
			})
			require.NoError(t, err)
			require.Len(t, ids, wantHidden)

			// The runs and the trial's log policy match.
			runs, err := api.SearchRuns(ctx, &apiv1.SearchRunsRequest{
				ProjectId: ptrs.Ptr(int32(projectID)),
			})
			require.NoError(t, err)
			require.Len(t, runs.Runs, 5)
			for _, r := range runs.Runs {
				hiddenRun := !vw.full && slices.Contains(hidden, r.Experiment.Id)
				require.Equal(t, hiddenRun, r.Experiment.Name == "", r.Experiment.Id)
				require.Equal(t, hiddenRun, r.Experiment.Description == "", r.Experiment.Id)
				require.Equal(t, hiddenRun, len(r.Labels) == 0, r.Experiment.Id)
				if r.Experiment.Id == int32(logsHidden.ID) {
					require.Equal(t, vw.full, r.LogPolicyMatched != nil)
				}
			}
			trials, err := api.GetExperimentTrials(ctx, &apiv1.GetExperimentTrialsRequest{
				ExperimentId: int32(logsHidden.ID),
			})
			require.NoError(t, err)
			require.Len(t, trials.Trials, 1)
			require.Equal(t, vw.full, trials.Trials[0].LogPolicyMatched != nil)
			trial, err := api.GetTrial(ctx, &apiv1.GetTrialRequest{TrialId: trials.Trials[0].Id})
			require.NoError(t, err)
			require.Equal(t, vw.full, trial.Trial.LogPolicyMatched != nil)

			// The labels of the project.
			labels, err := api.GetExperimentLabels(ctx, &apiv1.GetExperimentLabelsRequest{
				ProjectId: int32(projectID),
			})
			require.NoError(t, err)
			require.Contains(t, labels.Labels, "shared-label")
			require.Equal(t, vw.full, slices.Contains(labels.Labels, "secret-label"))
		})
	}

	// Sorting by a title puts hidden experiments last in either direction, in an order that does
	// not depend on their titles.
	orders := func(t *testing.T) [][]int32 {
		var out [][]int32
		for _, sortBy := range []apiv1.GetExperimentsRequest_SortBy{
			apiv1.GetExperimentsRequest_SORT_BY_NAME, apiv1.GetExperimentsRequest_SORT_BY_DESCRIPTION,
		} {
			for _, orderBy := range []apiv1.OrderBy{
				apiv1.OrderBy_ORDER_BY_ASC, apiv1.OrderBy_ORDER_BY_DESC,
			} {
				got, err := api.GetExperiments(otherCtx, &apiv1.GetExperimentsRequest{
					ProjectId: int32(projectID), SortBy: sortBy, OrderBy: orderBy,
				})
				require.NoError(t, err)
				var ids []int32
				for _, e := range got.Experiments {
					ids = append(ids, e.Id)
				}
				out = append(out, ids)
			}
		}
		for _, sortStr := range []string{
			"name=asc", "name=desc", "description=asc", "description=desc",
		} {
			got, err := api.SearchExperiments(otherCtx, &apiv1.SearchExperimentsRequest{
				ProjectId: ptrs.Ptr(int32(projectID)), Sort: ptrs.Ptr(sortStr),
			})
			require.NoError(t, err)
			var ids []int32
			for _, e := range got.Experiments {
				ids = append(ids, e.Experiment.Id)
			}
			out = append(out, ids)
		}
		for _, sortStr := range []string{
			"experimentName=asc", "experimentName=desc",
			"experimentDescription=asc", "experimentDescription=desc",
		} {
			got, err := api.SearchRuns(otherCtx, &apiv1.SearchRunsRequest{
				ProjectId: ptrs.Ptr(int32(projectID)), Sort: ptrs.Ptr(sortStr),
			})
			require.NoError(t, err)
			var ids []int32
			for _, r := range got.Runs {
				ids = append(ids, r.Experiment.Id)
			}
			out = append(out, ids)
		}
		for i, ids := range out {
			require.Len(t, ids, 5, i)
			require.ElementsMatch(t, hidden, ids[3:], "sort %d: %v", i, ids)
		}
		return out
	}
	before := orders(t)
	setExperimentTitle(adminCtx, t, hiddenA.ID, "z-secret", "zzz-secret-desc", "secret-label")
	setExperimentTitle(adminCtx, t, hiddenZ.ID, "a-secret", "aaa-secret-desc", "secret-label")
	require.Equal(t, before, orders(t))
}

// TestJobPrivacyLogReadPaths checks that every log read of a job whose logs another user may not
// read refuses that user with "logs not shared", whatever the request, while the owner and an
// admin read them; and that an open stream ends at the next recheck once the owner stops sharing.
func TestJobPrivacyLogReadPaths(t *testing.T) {
	var aux, compute string
	mockRM := mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute))
	api, admin, adminCtx := setupAPITest(t, nil, mockRM)
	lockJobPrivacyPolicy(t)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)
	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	ownerCtx, otherCtx := userContext(t, owner), userContext(t, other)

	projectID, _ := db.RequireMockProjectID(t, db.SingleDB(), 1, false)
	exp := createTestExpWithProjectID(t, api, owner, projectID)
	trial, trialTask := db.RequireMockTrial(t, db.SingleDB(), exp)
	_, err = db.Bun().NewRaw(`UPDATE tasks SET job_id = ? WHERE task_id = ?`,
		exp.JobID, trialTask.TaskID).Exec(adminCtx)
	require.NoError(t, err)
	// A trial task without a job, as before tasks had jobs, takes its experiment's.
	_, legacyTask := db.RequireMockTrial(t, db.SingleDB(), exp)
	_, err = db.Bun().NewRaw(`UPDATE tasks SET job_id = NULL WHERE task_id = ?`,
		legacyTask.TaskID).Exec(adminCtx)
	require.NoError(t, err)
	gcTask := &model.Task{
		TaskID: model.NewTaskID(), TaskType: model.TaskTypeCheckpointGC, JobID: &exp.JobID,
		StartTime: time.Now(), LogVersion: model.CurrentTaskLogVersion,
	}
	require.NoError(t, db.AddTask(adminCtx, gcTask))

	cmd, err := api.LaunchCommand(ownerCtx, &apiv1.LaunchCommandRequest{})
	require.NoError(t, err)
	shell, err := api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{})
	require.NoError(t, err)
	oldService := task.DefaultService
	task.DefaultService = &captureAllocationService{AllocationService: oldService}
	t.Cleanup(func() { task.DefaultService = oldService })
	generic, err := createGenericTaskForAccessTest(ownerCtx, t, api,
		&apiv1.CreateGenericTaskRequest{Config: "entrypoint: [\"true\"]\n"})
	require.NoError(t, err)

	taskIDs := map[string]model.TaskID{
		"trial":         trialTask.TaskID,
		"legacy trial":  legacyTask.TaskID,
		"experiment gc": gcTask.TaskID,
		"command":       model.TaskID(cmd.Command.Id),
		"shell":         model.TaskID(shell.Shell.Id),
		"generic":       model.TaskID(generic.TaskId),
	}
	for _, id := range taskIDs {
		require.NoError(t, api.m.db.AddTaskLogs([]*model.TaskLog{{
			TaskID: string(id), Log: "a secret line\n",
			Timestamp: ptrs.Ptr(time.Now().UTC()),
		}}))
	}
	for _, jobID := range []string{
		exp.JobID.String(), cmd.Command.JobId, shell.Shell.JobId,
		taskJobID(adminCtx, t, generic.TaskId).String(),
	} {
		shareJob(ownerCtx, t, api, jobID, true, false, true)
	}

	readTask := func(ctx context.Context, req *apiv1.TaskLogsRequest) (int, error) {
		stream := &mockStream[*apiv1.TaskLogsResponse]{ctx: ctx}
		err := api.TaskLogs(req, stream)
		return len(stream.getData()), err
	}
	for name, id := range taskIDs {
		t.Run(name, func(t *testing.T) {
			for _, req := range []*apiv1.TaskLogsRequest{
				{TaskId: string(id)},
				{TaskId: string(id), SearchText: "secret"},
				{TaskId: string(id), SearchText: "no such text"},
				{TaskId: string(id), Follow: true},
				{TaskId: string(id), Limit: 1, OrderBy: apiv1.OrderBy_ORDER_BY_DESC},
			} {
				_, err := readTask(otherCtx, req)
				requireStatus(t, err, codes.PermissionDenied, "logs not shared")
			}
			fields := &mockStream[*apiv1.TaskLogsFieldsResponse]{ctx: otherCtx}
			err := api.TaskLogsFields(&apiv1.TaskLogsFieldsRequest{TaskId: string(id)}, fields)
			requireStatus(t, err, codes.PermissionDenied, "logs not shared")

			for _, ctx := range []context.Context{ownerCtx, adminCtx} {
				n, err := readTask(ctx, &apiv1.TaskLogsRequest{TaskId: string(id)})
				require.NoError(t, err)
				require.Equal(t, 1, n)
			}
		})
	}

	trialLogs := &mockStream[*apiv1.TrialLogsResponse]{ctx: otherCtx}
	err = api.TrialLogs(&apiv1.TrialLogsRequest{TrialId: int32(trial.ID)}, trialLogs)
	requireStatus(t, err, codes.PermissionDenied, "logs not shared")
	trialFields := &mockStream[*apiv1.TrialLogsFieldsResponse]{ctx: otherCtx}
	err = api.TrialLogsFields(&apiv1.TrialLogsFieldsRequest{TrialId: int32(trial.ID)}, trialFields)
	requireStatus(t, err, codes.PermissionDenied, "logs not shared")
	ownerTrialLogs := &mockStream[*apiv1.TrialLogsResponse]{ctx: ownerCtx}
	require.NoError(t, api.TrialLogs(&apiv1.TrialLogsRequest{TrialId: int32(trial.ID)},
		ownerTrialLogs))
	require.NotEmpty(t, ownerTrialLogs.getData())

	// An open stream ends at the next recheck once the owner stops sharing logs.
	t.Run("recheck", func(t *testing.T) {
		shareJob(ownerCtx, t, api, exp.JobID.String(), true, true, true)
		oldPeriod := recheckAuthPeriod
		recheckAuthPeriod = 100 * time.Millisecond
		t.Cleanup(func() { recheckAuthPeriod = oldPeriod })

		ctx, cancel := context.WithCancel(otherCtx)
		defer cancel()
		stream := &mockStream[*apiv1.TaskLogsResponse]{ctx: ctx}
		done := make(chan error, 1)
		go func() {
			done <- api.TaskLogs(&apiv1.TaskLogsRequest{
				TaskId: string(trialTask.TaskID), Follow: true,
			}, stream)
		}()
		require.Eventually(t, func() bool { return len(stream.getData()) > 0 },
			10*time.Second, 50*time.Millisecond)
		select {
		case err := <-done:
			require.FailNow(t, "the stream ended while shared", "%v", err)
		case <-time.After(300 * time.Millisecond):
		}

		_, err := api.PatchMyJobPrivacy(ownerCtx, &apiv1.PatchMyJobPrivacyRequest{
			ShareLogs: ptrs.Ptr(false),
		})
		require.NoError(t, err)
		select {
		case err := <-done:
			require.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)
			require.Contains(t, err.Error(), "logs not shared")
		case <-time.After(10 * time.Second):
			require.FailNow(t, "the stream did not end")
		}
	})
}

// TestJobPrivacyTaskListsAndConfigs checks the queue, command, shell, generic task and task
// summary lists, the configs of commands, shells and generic tasks, and the experiment configs in
// checkpoints and model versions, as another user, the owner and an admin read them.
func TestJobPrivacyTaskListsAndConfigs(t *testing.T) {
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
	ownerCtx, otherCtx := userContext(t, owner), userContext(t, other)

	// An experiment with a checkpoint whose config is hidden, and a trial task whose title is.
	exp, ckptUUID := createGCTestExperiment(adminCtx, t, api, owner)
	shareJob(ownerCtx, t, api, exp.JobID.String(), true, true, false)
	hiddenExp := createTestExp(t, api, owner)
	shareJob(ownerCtx, t, api, hiddenExp.JobID.String(), false, false, false)
	_, hiddenTrialTask := db.RequireMockTrial(t, db.SingleDB(), hiddenExp)
	_, err = db.Bun().NewRaw(`UPDATE tasks SET job_id = ? WHERE task_id = ?`,
		hiddenExp.JobID, hiddenTrialTask.TaskID).Exec(adminCtx)
	require.NoError(t, err)

	descConfig := func(description string) *structpb.Struct {
		s, err := structpb.NewStruct(map[string]any{"description": description})
		require.NoError(t, err)
		return s
	}
	cmd, err := api.LaunchCommand(ownerCtx, &apiv1.LaunchCommandRequest{
		Config: descConfig(secretCommandDescription),
	})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, cmd.Command.JobId, false, false, false)
	shell, err := api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{
		Config: descConfig("shared-shell"),
	})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, shell.Shell.JobId, true, true, false)
	oldService := task.DefaultService
	task.DefaultService = &captureAllocationService{AllocationService: oldService}
	t.Cleanup(func() { task.DefaultService = oldService })
	generic, err := createGenericTaskForAccessTest(ownerCtx, t, api, &apiv1.CreateGenericTaskRequest{
		Config: "name: secret-generic\ndescription: secret-generic-desc\nentrypoint: [\"true\"]\n",
	})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, taskJobID(adminCtx, t, generic.TaskId).String(), false, false, false)

	mockRM.On("GetExternalJobs", mock.Anything).Return([]*jobv1.Job{{
		JobId: hiddenExp.JobID.String(), Name: secretJobName, SubmissionTime: timestamppb.Now(),
		UserId: int32(owner.ID),
	}}, nil)
	summaries = map[model.AllocationID]sproto.AllocationSummary{
		model.AllocationID(hiddenTrialTask.TaskID + ".1"): {
			TaskID: hiddenTrialTask.TaskID, AllocationID: model.AllocationID(hiddenTrialTask.TaskID + ".1"),
			Name: "secret-allocation", RegisteredTime: time.Now(), ResourcePool: "default",
		},
	}

	// A model of the other user's, whose version is the owner's checkpoint.
	modelName := "privacy-" + ckptUUID[:8]
	_, err = api.PostModel(otherCtx, &apiv1.PostModelRequest{Name: modelName})
	require.NoError(t, err)
	posted, err := api.PostModelVersion(otherCtx, &apiv1.PostModelVersionRequest{
		ModelName: modelName, CheckpointUuid: ckptUUID,
	})
	require.NoError(t, err)

	viewers := map[string]struct {
		user model.User
		full bool
	}{
		"owner": {owner, true},
		"admin": {admin, true},
		"other": {other, false},
	}
	for name, vw := range viewers {
		t.Run(name, func(t *testing.T) {
			ctx := userContext(t, vw.user)
			jobs, err := api.GetJobs(ctx, &apiv1.GetJobsRequest{ResourcePool: "default"})
			require.NoError(t, err)
			require.Len(t, jobs.Jobs, 1)
			require.Equal(t, vw.full, jobs.Jobs[0].Name == secretJobName)
			jobsV2, err := api.GetJobsV2(ctx, &apiv1.GetJobsV2Request{ResourcePool: "default"})
			require.NoError(t, err)
			require.Len(t, jobsV2.Jobs, 1)
			require.Equal(t, vw.full, jobsV2.Jobs[0].GetFull().Name == secretJobName)
			require.Equal(t, int32(owner.ID), jobsV2.Jobs[0].GetFull().UserId)

			tasks, err := api.GetTasks(ctx, &apiv1.GetTasksRequest{})
			require.NoError(t, err)
			require.Len(t, tasks.AllocationIdToSummary, 1)
			for _, s := range tasks.AllocationIdToSummary {
				require.Equal(t, vw.full, s.Name == "secret-allocation")
				require.Equal(t, "default", s.ResourcePool)
			}

			cmds, err := api.GetCommands(ctx, &apiv1.GetCommandsRequest{})
			require.NoError(t, err)
			for _, c := range cmds.Commands {
				if c.Id == cmd.Command.Id {
					require.Equal(t, vw.full, c.Description == secretCommandDescription)
				}
			}
			getCmd, err := api.GetCommand(ctx, &apiv1.GetCommandRequest{CommandId: cmd.Command.Id})
			require.NoError(t, err)
			require.Equal(t, vw.full, getCmd.Command.Description == secretCommandDescription)
			require.Equal(t, vw.full, getCmd.Config != nil)
			require.Equal(t, int32(owner.ID), getCmd.Command.UserId)

			getShell, err := api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: shell.Shell.Id})
			require.NoError(t, err)
			require.Equal(t, "shared-shell", getShell.Shell.Description)
			require.Equal(t, vw.full, getShell.Config != nil)

			generics, err := api.GetGenericTasks(ctx, &apiv1.GetGenericTasksRequest{
				TaskIds: []string{generic.TaskId},
			})
			require.NoError(t, err)
			require.Len(t, generics.Tasks, 1)
			require.Equal(t, vw.full, generics.Tasks[0].Name == "secret-generic")
			require.Equal(t, vw.full, generics.Tasks[0].Description == "secret-generic-desc")
			for search, want := range map[string]bool{
				"secret": vw.full, generic.TaskId[:8]: true,
			} {
				found, err := api.GetGenericTasks(ctx, &apiv1.GetGenericTasksRequest{
					TaskIds: []string{generic.TaskId}, Search: search,
				})
				require.NoError(t, err)
				require.Equal(t, want, len(found.Tasks) == 1, search)
			}

			config, err := api.GetGenericTaskConfig(ctx, &apiv1.GetGenericTaskConfigRequest{
				TaskId: generic.TaskId,
			})
			if vw.full {
				require.NoError(t, err)
				require.Contains(t, config.Config, "secret-generic")
			} else {
				requireStatus(t, err, codes.PermissionDenied, "config not shared")
				_, err = api.CreateGenericTask(ctx, &apiv1.CreateGenericTaskRequest{
					ForkedFrom: ptrs.Ptr(generic.TaskId),
				})
				requireStatus(t, err, codes.PermissionDenied, "config not shared")
			}

			// Checkpoints keep their storage, and their hyperparameters, when the config is hidden.
			requireCheckpointConfig := func(cfg *structpb.Struct, msg string) {
				t.Helper()
				require.NotNil(t, cfg, msg)
				require.Contains(t, structKeys(cfg), "checkpoint_storage", msg)
				if vw.full {
					require.Contains(t, structKeys(cfg), "searcher", msg)
				} else {
					require.Equal(t, []string{"checkpoint_storage"}, structKeys(cfg), msg)
				}
			}
			ckpt, err := api.GetCheckpoint(ctx, &apiv1.GetCheckpointRequest{CheckpointUuid: ckptUUID})
			require.NoError(t, err)
			requireCheckpointConfig(ckpt.Checkpoint.Training.ExperimentConfig, "GetCheckpoint")
			expCkpts, err := api.GetExperimentCheckpoints(ctx, &apiv1.GetExperimentCheckpointsRequest{
				Id: int32(exp.ID),
			})
			require.NoError(t, err)
			require.Len(t, expCkpts.Checkpoints, 1)
			requireCheckpointConfig(expCkpts.Checkpoints[0].Training.ExperimentConfig,
				"GetExperimentCheckpoints")
			trialCkpts, err := api.GetTrialCheckpoints(ctx, &apiv1.GetTrialCheckpointsRequest{
				Id: ckpt.Checkpoint.Training.GetTrialId(),
			})
			require.NoError(t, err)
			require.Len(t, trialCkpts.Checkpoints, 1)
			requireCheckpointConfig(trialCkpts.Checkpoints[0].Training.ExperimentConfig,
				"GetTrialCheckpoints")
			mv, err := api.GetModelVersion(ctx, &apiv1.GetModelVersionRequest{
				ModelName: modelName, ModelVersionNum: posted.ModelVersion.Version,
			})
			require.NoError(t, err)
			requireCheckpointConfig(mv.ModelVersion.Checkpoint.Training.ExperimentConfig,
				"GetModelVersion")
			mvs, err := api.GetModelVersions(ctx, &apiv1.GetModelVersionsRequest{
				ModelName: modelName,
			})
			require.NoError(t, err)
			require.Len(t, mvs.ModelVersions, 1)
			requireCheckpointConfig(mvs.ModelVersions[0].Checkpoint.Training.ExperimentConfig,
				"GetModelVersions")
		})
	}

	// The model's owner writes the versions; the experiment's config is still not theirs to read.
	require.Equal(t, []string{"checkpoint_storage"},
		structKeys(posted.ModelVersion.Checkpoint.Training.ExperimentConfig), "PostModelVersion")
	patched, err := api.PatchModelVersion(otherCtx, &apiv1.PatchModelVersionRequest{
		ModelName: modelName, ModelVersionNum: posted.ModelVersion.Version,
		ModelVersion: &modelv1.PatchModelVersion{Comment: wrapperspb.String("patched")},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"checkpoint_storage"},
		structKeys(patched.ModelVersion.Checkpoint.Training.ExperimentConfig), "PatchModelVersion")
	meta, err := api.PostCheckpointMetadata(ownerCtx, &apiv1.PostCheckpointMetadataRequest{
		Checkpoint: &checkpointv1.Checkpoint{Uuid: ckptUUID},
	})
	require.NoError(t, err)
	require.Contains(t, structKeys(meta.Checkpoint.Training.ExperimentConfig), "searcher")

	// The raw usage labels follow the title.
	hiddenLabeled := createTestExp(t, api, owner, "secret-label")
	shareJob(ownerCtx, t, api, hiddenLabeled.JobID.String(), false, false, false)
	sharedLabeled := createTestExp(t, api, owner, "shared-label")
	for user, full := range map[*model.User]bool{&owner: true, &admin: true, &other: false} {
		entries := []*masterv1.ResourceAllocationRawEntry{
			{ExperimentId: int32(hiddenLabeled.ID), Labels: []string{"secret-label"}},
			{ExperimentId: int32(sharedLabeled.ID), Labels: []string{"shared-label"}},
		}
		require.NoError(t, redactRawAllocationLabels(adminCtx, *user, entries))
		require.Equal(t, full, len(entries[0].Labels) == 1, user.Username)
		require.Equal(t, []string{"shared-label"}, entries[1].Labels, user.Username)
	}
}

// TestJobPrivacyListSQLMatchesGo checks that the capabilities a list computes in SQL equal those
// the rule computes in Go for the same jobs, for every viewer.
func TestJobPrivacyListSQLMatchesGo(t *testing.T) {
	api, admin, adminCtx := setupAPITest(t, nil)
	lockJobPrivacyPolicy(t)
	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	ownerCtx := userContext(t, owner)
	projectID, _ := db.RequireMockProjectID(t, db.SingleDB(), 1, false)
	var exps []*model.Experiment
	sharings := [][3]bool{
		{true, true, true},
		{false, false, false},
		{true, false, true},
		{true, true, false},
		{true, false, false},
	}
	for i, sharing := range sharings {
		exp := createTestExpWithProjectID(t, api, owner, projectID)
		db.RequireMockTrial(t, db.SingleDB(), exp)
		if i > 0 {
			shareJob(ownerCtx, t, api, exp.JobID.String(), sharing[0], sharing[1], sharing[2])
		}
		exps = append(exps, exp)
	}
	// The owner keeps titles to themselves for one round.
	for _, shareTitles := range []bool{true, false} {
		_, err := api.PatchMyJobPrivacy(ownerCtx, &apiv1.PatchMyJobPrivacyRequest{
			ShareTitles: ptrs.Ptr(shareTitles),
		})
		require.NoError(t, err)
		for _, u := range []model.User{owner, other, admin} {
			ctx := userContext(t, u)
			v, err := jobprivacy.NewViewer(adminCtx, u)
			require.NoError(t, err)
			var jobIDs []model.JobID
			for _, e := range exps {
				jobIDs = append(jobIDs, e.JobID)
			}
			want, err := jobprivacy.ForJobs(adminCtx, v, jobIDs)
			require.NoError(t, err)
			byJob := func(jobID string) *jobv1.JobCapabilities {
				return want[model.JobID(jobID)].Proto()
			}

			list, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{
				ProjectId: int32(projectID),
			})
			require.NoError(t, err)
			require.Len(t, list.Experiments, len(exps))
			for _, e := range list.Experiments {
				require.Equal(t, byJob(e.JobId), e.Capabilities, "GetExperiments %s", u.Username)
			}
			search, err := api.SearchExperiments(ctx, &apiv1.SearchExperimentsRequest{
				ProjectId: ptrs.Ptr(int32(projectID)),
			})
			require.NoError(t, err)
			for _, e := range search.Experiments {
				require.Equal(t, byJob(e.Experiment.JobId), e.Experiment.Capabilities,
					"SearchExperiments %s", u.Username)
			}
			runs, err := api.SearchRuns(ctx, &apiv1.SearchRunsRequest{
				ProjectId: ptrs.Ptr(int32(projectID)),
			})
			require.NoError(t, err)
			require.Len(t, runs.Runs, len(exps))
			jobOf := map[int32]string{}
			for _, e := range exps {
				jobOf[int32(e.ID)] = e.JobID.String()
			}
			for _, r := range runs.Runs {
				require.Equal(t, byJob(jobOf[r.Experiment.Id]), r.Experiment.Capabilities,
					"SearchRuns %s", u.Username)
			}
		}
	}
}
