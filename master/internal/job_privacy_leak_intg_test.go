//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/exp/maps"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/cluster"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/checkpointv1"
	"github.com/determined-ai/determined/proto/pkg/commonv1"
	"github.com/determined-ai/determined/proto/pkg/experimentv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
	"github.com/determined-ai/determined/proto/pkg/logv1"
	"github.com/determined-ai/determined/proto/pkg/masterv1"
	"github.com/determined-ai/determined/proto/pkg/modelv1"
)

// The titles of the hidden tasks of the list tests.
const (
	secretJobName            = "secret-job"
	secretCommandDescription = "secret-command"
	secretShellDescription   = "secret-shell"
	secretAllocationName     = "secret-allocation"
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

// echoContextAs is the context of an Echo GET of target by user, with the route's path
// parameters, and the recorder of its response.
func echoContextAs(
	user model.User, target string, params map[string]string,
) (echo.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c := &detContext.DetContext{
		Context: echo.New().NewContext(httptest.NewRequest(http.MethodGet, target, nil), rec),
	}
	c.SetUser(user)
	var names, values []string
	for k, v := range params {
		names = append(names, k)
		values = append(values, v)
	}
	c.SetParamNames(names...)
	c.SetParamValues(values...)
	return c, rec
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

			// The project's search returns the same columns.
			searched, err := api.SearchExperiments(ctx, &apiv1.SearchExperimentsRequest{
				ProjectId: ptrs.Ptr(int32(projectID)),
			})
			require.NoError(t, err)
			require.Len(t, searched.Experiments, 5)
			for _, se := range searched.Experiments {
				e := se.Experiment
				require.Equal(t, int32(owner.ID), e.UserId)
				require.Equal(t, "kubernetes", e.ResourcePool)
				switch {
				case vw.full:
					require.NotEmpty(t, e.Name)
					require.NotEmpty(t, e.Description)
					require.NotEmpty(t, e.Labels)
					require.Contains(t, structKeys(e.Config), "searcher") //nolint:staticcheck
					require.Equal(t, full, e.Capabilities)
				case slices.Contains(hidden, e.Id):
					require.Empty(t, e.Name)
					require.Empty(t, e.Description)
					require.Empty(t, e.Labels)
					require.Empty(t, e.Notes)
					requireResourcesOnly(t, e.Config, "search hidden") //nolint:staticcheck
					require.Equal(t, none, e.Capabilities)
				case e.Id == int32(configHidden.ID):
					require.Equal(t, "c-shared", e.Name)
					require.Equal(t, "shared-desc", e.Description)
					requireResourcesOnly(t, e.Config, "search config hidden") //nolint:staticcheck
					require.Equal(t,
						&jobv1.JobCapabilities{ViewTitle: true, ViewLogs: true}, e.Capabilities)
				default:
					require.NotEmpty(t, e.Name)
					require.NotEmpty(t, e.Labels)
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

// countingTaskLogBackend counts the task log counts that reach the backend.
type countingTaskLogBackend struct {
	TaskLogBackend
	counts atomic.Int64
}

// TaskLogsCount counts the call and passes it on.
func (b *countingTaskLogBackend) TaskLogsCount(
	taskID model.TaskID, filters []api.Filter,
) (int, error) {
	b.counts.Add(1)
	return b.TaskLogBackend.TaskLogsCount(taskID, filters)
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
	// A trial from before task logs, whose logs are in trial_logs.
	v0Trial, v0Task := db.RequireMockTrial(t, db.SingleDB(), exp)
	_, err = db.Bun().NewRaw(`UPDATE tasks SET job_id = ?, log_version = 0 WHERE task_id = ?`,
		exp.JobID, v0Task.TaskID).Exec(adminCtx)
	require.NoError(t, err)
	_, err = db.Bun().NewRaw(`INSERT INTO trial_logs (trial_id, message, level, timestamp)
		VALUES (?, convert_to('a secret v0 line', 'UTF8'), 'INFO', now())`, v0Trial.ID).
		Exec(adminCtx)
	require.NoError(t, err)
	counting := &countingTaskLogBackend{TaskLogBackend: api.m.taskLogBackend}
	api.m.taskLogBackend = counting

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
			// No log work ran for the refused requests, the count included.
			require.Zero(t, counting.counts.Load())

			for _, ctx := range []context.Context{ownerCtx, adminCtx} {
				n, err := readTask(ctx, &apiv1.TaskLogsRequest{TaskId: string(id)})
				require.NoError(t, err)
				require.Equal(t, 1, n)
			}
			counting.counts.Store(0)
		})
	}

	// Trial logs, on the current and the legacy path, whatever the request.
	for _, trialID := range []int{trial.ID, v0Trial.ID} {
		for _, req := range []*apiv1.TrialLogsRequest{
			{TrialId: int32(trialID)},
			{TrialId: int32(trialID), Follow: true},
			{TrialId: int32(trialID), SearchText: "secret"},
			{TrialId: int32(trialID), SearchText: "no such text"},
			{TrialId: int32(trialID), Limit: 1, OrderBy: apiv1.OrderBy_ORDER_BY_DESC},
			{TrialId: int32(trialID), Levels: []logv1.LogLevel{logv1.LogLevel_LOG_LEVEL_INFO}},
		} {
			stream := &mockStream[*apiv1.TrialLogsResponse]{ctx: otherCtx}
			err = api.TrialLogs(req, stream)
			requireStatus(t, err, codes.PermissionDenied, "logs not shared")
			require.Empty(t, stream.getData())
		}
		for _, follow := range []bool{false, true} {
			fields := &mockStream[*apiv1.TrialLogsFieldsResponse]{ctx: otherCtx}
			err = api.TrialLogsFields(&apiv1.TrialLogsFieldsRequest{
				TrialId: int32(trialID), Follow: follow,
			}, fields)
			requireStatus(t, err, codes.PermissionDenied, "logs not shared")
		}
		require.Zero(t, counting.counts.Load())
		for _, ctx := range []context.Context{ownerCtx, adminCtx} {
			stream := &mockStream[*apiv1.TrialLogsResponse]{ctx: ctx}
			require.NoError(t, api.TrialLogs(&apiv1.TrialLogsRequest{TrialId: int32(trialID)},
				stream))
			require.Len(t, stream.getData(), 1, trialID)
		}
		counting.counts.Store(0)
	}

	// An open stream ends at the next recheck once the owner stops sharing logs: task logs, trial
	// logs on the legacy path and trial log fields.
	t.Run("recheck", func(t *testing.T) {
		shareJob(ownerCtx, t, api, exp.JobID.String(), true, true, true)
		oldPeriod, oldWait := recheckAuthPeriod, distinctFieldBatchWaitTime
		recheckAuthPeriod = 100 * time.Millisecond
		distinctFieldBatchWaitTime = 100 * time.Millisecond
		t.Cleanup(func() { recheckAuthPeriod, distinctFieldBatchWaitTime = oldPeriod, oldWait })

		ctx, cancel := context.WithCancel(otherCtx)
		defer cancel()
		taskStream := &mockStream[*apiv1.TaskLogsResponse]{ctx: ctx}
		v0Stream := &mockStream[*apiv1.TrialLogsResponse]{ctx: ctx}
		fieldsStream := &mockStream[*apiv1.TrialLogsFieldsResponse]{ctx: ctx}
		streams := map[string]func() error{
			"task logs": func() error {
				return api.TaskLogs(&apiv1.TaskLogsRequest{
					TaskId: string(trialTask.TaskID), Follow: true,
				}, taskStream)
			},
			"legacy trial logs": func() error {
				return api.TrialLogs(&apiv1.TrialLogsRequest{
					TrialId: int32(v0Trial.ID), Follow: true,
				}, v0Stream)
			},
			"trial log fields": func() error {
				return api.TrialLogsFields(&apiv1.TrialLogsFieldsRequest{
					TrialId: int32(trial.ID), Follow: true,
				}, fieldsStream)
			},
		}
		done := map[string]chan error{}
		for name, run := range streams {
			done[name] = make(chan error, 1)
			go func() { done[name] <- run() }()
		}
		require.Eventually(t, func() bool {
			return len(taskStream.getData()) > 0 && len(v0Stream.getData()) > 0 &&
				len(fieldsStream.getData()) > 0
		}, 10*time.Second, 50*time.Millisecond)
		time.Sleep(300 * time.Millisecond)
		for name, ch := range done {
			select {
			case err := <-ch:
				require.FailNow(t, "a stream ended while shared", "%s: %v", name, err)
			default:
			}
		}

		_, err := api.PatchMyJobPrivacy(ownerCtx, &apiv1.PatchMyJobPrivacyRequest{
			ShareLogs: ptrs.Ptr(false),
		})
		require.NoError(t, err)
		for name, ch := range done {
			select {
			case err := <-ch:
				require.Equal(t, codes.PermissionDenied, status.Code(err), "%s: %v", name, err)
				require.Contains(t, err.Error(), "logs not shared", name)
			case <-time.After(10 * time.Second):
				require.FailNow(t, "the stream did not end", name)
			}
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
	// Another, whose checkpoint is in no model, so that the GC preview lists it.
	gcExp, _ := createGCTestExperiment(adminCtx, t, api, owner)
	shareJob(ownerCtx, t, api, gcExp.JobID.String(), true, true, false)
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
	hiddenShell, err := api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{
		Config: descConfig(secretShellDescription),
	})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, hiddenShell.Shell.JobId, false, false, false)
	setExperimentTitle(adminCtx, t, hiddenExp.ID, "secret-model-def", "secret-desc")
	oldService := task.DefaultService
	task.DefaultService = &captureAllocationService{AllocationService: oldService}
	t.Cleanup(func() { task.DefaultService = oldService })
	generic, err := createGenericTaskForAccessTest(ownerCtx, t, api, &apiv1.CreateGenericTaskRequest{
		Config: "name: secret-generic\ndescription: secret-generic-desc\nentrypoint: [\"true\"]\n",
	})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, taskJobID(adminCtx, t, generic.TaskId).String(), false, false, false)

	// Each call gets jobs of its own, as the job service builds them, so that one viewer's
	// redaction never reaches another's response.
	mockRM.On("GetExternalJobs", mock.Anything).Return(
		func(rm.ResourcePoolName) []*jobv1.Job {
			return []*jobv1.Job{{
				JobId: hiddenExp.JobID.String(), Name: secretJobName,
				SubmissionTime: timestamppb.Now(), UserId: int32(owner.ID),
			}}
		}, nil)
	summaries = map[model.AllocationID]sproto.AllocationSummary{
		model.AllocationID(hiddenTrialTask.TaskID + ".1"): {
			TaskID: hiddenTrialTask.TaskID, AllocationID: model.AllocationID(hiddenTrialTask.TaskID + ".1"),
			Name: secretAllocationName, RegisteredTime: time.Now(), ResourcePool: "default",
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
				require.Equal(t, vw.full, s.Name == secretAllocationName)
				require.Equal(t, "default", s.ResourcePool)
			}

			// The Echo twin of GetTasks omits the same names.
			tasksCtx, _ := echoContextAs(vw.user, "/tasks", nil)
			echoTasks, err := api.m.getTasks(tasksCtx)
			require.NoError(t, err)
			echoSummaries := echoTasks.(map[model.AllocationID]sproto.AllocationSummary)
			require.Len(t, echoSummaries, 1)
			for _, s := range echoSummaries {
				require.Equal(t, vw.full, s.Name == secretAllocationName)
				require.Equal(t, "default", s.ResourcePool)
			}

			cmds, err := api.GetCommands(ctx, &apiv1.GetCommandsRequest{})
			require.NoError(t, err)
			found := false
			for _, c := range cmds.Commands {
				if c.Id == cmd.Command.Id {
					found = true
					require.Equal(t, vw.full, c.Description == secretCommandDescription)
				}
			}
			require.True(t, found)
			getCmd, err := api.GetCommand(ctx, &apiv1.GetCommandRequest{CommandId: cmd.Command.Id})
			require.NoError(t, err)
			require.Equal(t, vw.full, getCmd.Command.Description == secretCommandDescription)
			require.Equal(t, vw.full, getCmd.Config != nil)
			require.Equal(t, int32(owner.ID), getCmd.Command.UserId)

			getShell, err := api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: shell.Shell.Id})
			require.NoError(t, err)
			require.Equal(t, "shared-shell", getShell.Shell.Description)
			require.Equal(t, vw.full, getShell.Config != nil)
			getHiddenShell, err := api.GetShell(ctx, &apiv1.GetShellRequest{
				ShellId: hiddenShell.Shell.Id,
			})
			require.NoError(t, err)
			require.Equal(t, vw.full, getHiddenShell.Shell.Description == secretShellDescription)
			require.Equal(t, vw.full, getHiddenShell.Shell.Description != "")
			require.Equal(t, vw.full, getHiddenShell.Config != nil)
			require.Equal(t, int32(owner.ID), getHiddenShell.Shell.UserId)
			shells, err := api.GetShells(ctx, &apiv1.GetShellsRequest{})
			require.NoError(t, err)
			found = false
			for _, s := range shells.Shells {
				switch s.Id {
				case hiddenShell.Shell.Id:
					found = true
					require.Equal(t, vw.full, s.Description == secretShellDescription)
					require.Equal(t, vw.full, s.Description != "")
				case shell.Shell.Id:
					require.Equal(t, "shared-shell", s.Description)
				}
			}
			require.True(t, found)

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

			// The checkpoint GC preview reduces the config as the checkpoint RPCs do.
			previewCtx, _ := echoContextAs(vw.user,
				fmt.Sprintf("/experiments/%d/preview_gc?save_experiment_best=0&"+
					"save_trial_best=0&save_trial_latest=0", gcExp.ID),
				map[string]string{"experiment_id": strconv.Itoa(gcExp.ID)})
			preview, err := api.m.getExperimentCheckpointsToGC(previewCtx)
			require.NoError(t, err)
			previewJSON, err := json.Marshal(preview)
			require.NoError(t, err)
			var previewed struct {
				Checkpoints []struct {
					ExperimentConfig map[string]any `json:"ExperimentConfig"`
				} `json:"checkpoints"`
			}
			require.NoError(t, json.Unmarshal(previewJSON, &previewed), string(previewJSON))
			require.Len(t, previewed.Checkpoints, 1, string(previewJSON))
			previewKeys := maps.Keys(previewed.Checkpoints[0].ExperimentConfig)
			sort.Strings(previewKeys)
			require.Contains(t, previewKeys, "checkpoint_storage", "preview_gc")
			if vw.full {
				require.Contains(t, previewKeys, "searcher", "preview_gc")
			} else {
				require.Equal(t, []string{"checkpoint_storage"}, previewKeys, "preview_gc")
			}

			// The model definition's file is named after the experiment only for those who may
			// read its title.
			defCtx, rec := echoContextAs(vw.user,
				fmt.Sprintf("/experiments/%d/model_def", hiddenExp.ID),
				map[string]string{"experiment_id": strconv.Itoa(hiddenExp.ID)})
			require.NoError(t, api.m.getExperimentModelDefinition(defCtx))
			disposition := rec.Header().Get("Content-Disposition")
			if vw.full {
				require.Contains(t, disposition, "secret-model-def")
			} else {
				require.Equal(t, fmt.Sprintf(`attachment; filename="exp%d_model_def.tar.gz"`,
					hiddenExp.ID), disposition)
			}
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

// TestJobPrivacyCommandAndShellSort checks that a command or shell whose title another user may
// not read sorts, for that user, where a row with an empty description would: before the others
// in ascending order and after them in descending order, ties by ID, whatever its title.
func TestJobPrivacyCommandAndShellSort(t *testing.T) {
	var aux, compute string
	mockRM := mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute))
	api, admin, _ := setupAPITest(t, nil, mockRM)
	lockJobPrivacyPolicy(t)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)
	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	ownerCtx, otherCtx := userContext(t, owner), userContext(t, other)

	descConfig := func(description string) *structpb.Struct {
		s, err := structpb.NewStruct(map[string]any{"description": description})
		require.NoError(t, err)
		return s
	}
	// Two hidden rows whose titles would sort first and last, and a shared one between them.
	var cmdHidden, shellHidden []string
	var cmdShared, shellShared string
	for _, title := range []string{"a-secret", "m-shared", "z-secret"} {
		hidden := title != "m-shared"
		cmd, err := api.LaunchCommand(ownerCtx, &apiv1.LaunchCommandRequest{
			Config: descConfig(title),
		})
		require.NoError(t, err)
		shell, err := api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{
			Config: descConfig(title),
		})
		require.NoError(t, err)
		shareJob(ownerCtx, t, api, cmd.Command.JobId, !hidden, !hidden, !hidden)
		shareJob(ownerCtx, t, api, shell.Shell.JobId, !hidden, !hidden, !hidden)
		if hidden {
			cmdHidden = append(cmdHidden, cmd.Command.Id)
			shellHidden = append(shellHidden, shell.Shell.Id)
		} else {
			cmdShared, shellShared = cmd.Command.Id, shell.Shell.Id
		}
	}
	// want is the order of rows with empty descriptions, ties by ID, around the shared row.
	want := func(hidden []string, shared string, order apiv1.OrderBy) []string {
		ids := slices.Clone(hidden)
		sort.Strings(ids)
		if order == apiv1.OrderBy_ORDER_BY_DESC {
			slices.Reverse(ids)
			return append([]string{shared}, ids...)
		}
		return append(ids, shared)
	}
	ours := func(ids []string, keep ...string) []string {
		var out []string
		for _, id := range ids {
			if slices.Contains(keep, id) {
				out = append(out, id)
			}
		}
		return out
	}
	for _, order := range []apiv1.OrderBy{apiv1.OrderBy_ORDER_BY_ASC, apiv1.OrderBy_ORDER_BY_DESC} {
		cmds, err := api.GetCommands(otherCtx, &apiv1.GetCommandsRequest{
			SortBy: apiv1.GetCommandsRequest_SORT_BY_DESCRIPTION, OrderBy: order,
		})
		require.NoError(t, err)
		var cmdOrder []string
		for _, c := range cmds.Commands {
			cmdOrder = append(cmdOrder, c.Id)
		}
		require.Equal(t, want(cmdHidden, cmdShared, order),
			ours(cmdOrder, append(cmdHidden, cmdShared)...), order)

		shells, err := api.GetShells(otherCtx, &apiv1.GetShellsRequest{
			SortBy: apiv1.GetShellsRequest_SORT_BY_DESCRIPTION, OrderBy: order,
		})
		require.NoError(t, err)
		var shellOrder []string
		for _, s := range shells.Shells {
			shellOrder = append(shellOrder, s.Id)
		}
		require.Equal(t, want(shellHidden, shellShared, order),
			ours(shellOrder, append(shellHidden, shellShared)...), order)
	}
}

// TestJobPrivacyControlResponses checks that the responses of the RPCs that change a job carry
// the caller's capabilities and omit what they hide. In basic authorization only the owner and
// admins reach them, so both get everything; under RBAC a workspace editor gets the redaction of
// the read paths.
func TestJobPrivacyControlResponses(t *testing.T) {
	var aux, compute string
	mockRM := mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute))
	mockRM.On("Release", mock.Anything).Return()
	api, admin, _ := setupAPITest(t, nil, mockRM)
	lockJobPrivacyPolicy(t)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)
	owner := addGCTestOwner(t)
	ownerCtx := userContext(t, owner)
	full := jobprivacy.AllCaps.Proto()

	descConfig, err := structpb.NewStruct(map[string]any{"description": "secret-control"})
	require.NoError(t, err)
	exp := createTestExp(t, api, owner)
	shareJob(ownerCtx, t, api, exp.JobID.String(), false, false, false)

	for name, u := range map[string]model.User{"owner": owner, "admin": admin} {
		t.Run(name, func(t *testing.T) {
			ctx := userContext(t, u)
			cmd, err := api.LaunchCommand(ownerCtx, &apiv1.LaunchCommandRequest{Config: descConfig})
			require.NoError(t, err)
			shareJob(ownerCtx, t, api, cmd.Command.JobId, false, false, false)
			shell, err := api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{Config: descConfig})
			require.NoError(t, err)
			shareJob(ownerCtx, t, api, shell.Shell.JobId, false, false, false)

			prio, err := api.SetCommandPriority(ctx, &apiv1.SetCommandPriorityRequest{
				CommandId: cmd.Command.Id, Priority: 10,
			})
			require.NoError(t, err)
			require.Equal(t, "secret-control", prio.Command.Description)
			require.Equal(t, full, prio.Command.Capabilities, "SetCommandPriority")
			killed, err := api.KillCommand(ctx, &apiv1.KillCommandRequest{CommandId: cmd.Command.Id})
			require.NoError(t, err)
			require.Equal(t, "secret-control", killed.Command.Description)
			require.Equal(t, full, killed.Command.Capabilities, "KillCommand")

			shellPrio, err := api.SetShellPriority(ctx, &apiv1.SetShellPriorityRequest{
				ShellId: shell.Shell.Id, Priority: 10,
			})
			require.NoError(t, err)
			require.Equal(t, "secret-control", shellPrio.Shell.Description)
			require.Equal(t, full, shellPrio.Shell.Capabilities, "SetShellPriority")
			killedShell, err := api.KillShell(ctx, &apiv1.KillShellRequest{ShellId: shell.Shell.Id})
			require.NoError(t, err)
			require.Equal(t, "secret-control", killedShell.Shell.Description)
			require.Equal(t, full, killedShell.Shell.Capabilities, "KillShell")

			patched, err := api.PatchExperiment(ctx, &apiv1.PatchExperimentRequest{
				Experiment: &experimentv1.PatchExperiment{
					Id: int32(exp.ID), Notes: wrapperspb.String("notes of " + name),
				},
			})
			require.NoError(t, err)
			require.NotEmpty(t, patched.Experiment.Name)
			require.Equal(t, "notes of "+name, patched.Experiment.Notes)
			require.Equal(t, full, patched.Experiment.Capabilities, "PatchExperiment")
		})
	}
}

// jobPrivacyFailingAuthZ is a cluster authorization whose admin predicate fails.
type jobPrivacyFailingAuthZ struct {
	*cluster.MiscAuthZBasic
}

// CanUpdateMasterConfig fails.
func (jobPrivacyFailingAuthZ) CanUpdateMasterConfig(
	context.Context, *model.User,
) (permErr error, err error) {
	return nil, errors.New("the authorization backend is down")
}

var registerJobPrivacyFailingAuthZ sync.Once

// TestJobPrivacyFailClosed checks that the read paths answer nothing when job privacy cannot be
// decided: an admin predicate error is Internal and a policy read error is Unavailable, with no
// partial list, and an admin, who reads no policy, is unaffected by the latter.
func TestJobPrivacyFailClosed(t *testing.T) {
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

	projectID, _ := db.RequireMockProjectID(t, db.SingleDB(), 1, false)
	exp := createTestExpWithProjectID(t, api, owner, projectID)
	_, trialTask := db.RequireMockTrial(t, db.SingleDB(), exp)
	_, err = db.Bun().NewRaw(`UPDATE tasks SET job_id = ? WHERE task_id = ?`,
		exp.JobID, trialTask.TaskID).Exec(adminCtx)
	require.NoError(t, err)
	_, err = api.LaunchCommand(ownerCtx, &apiv1.LaunchCommandRequest{})
	require.NoError(t, err)
	_, err = api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{})
	require.NoError(t, err)
	oldService := task.DefaultService
	task.DefaultService = &captureAllocationService{AllocationService: oldService}
	t.Cleanup(func() { task.DefaultService = oldService })
	generic, err := createGenericTaskForAccessTest(ownerCtx, t, api,
		&apiv1.CreateGenericTaskRequest{Config: "entrypoint: [\"true\"]\n"})
	require.NoError(t, err)
	mockRM.On("GetExternalJobs", mock.Anything).Return(func(rm.ResourcePoolName) []*jobv1.Job {
		return []*jobv1.Job{{JobId: exp.JobID.String(), SubmissionTime: timestamppb.Now()}}
	}, nil)
	summaries = map[model.AllocationID]sproto.AllocationSummary{
		model.AllocationID(trialTask.TaskID + ".1"): {
			TaskID: trialTask.TaskID, AllocationID: model.AllocationID(trialTask.TaskID + ".1"),
			RegisteredTime: time.Now(), ResourcePool: "default",
		},
	}

	// reads makes each read as the user and returns its response, nil when there is none, and
	// its error.
	reads := func(ctx context.Context) map[string]func() (any, error) {
		return map[string]func() (any, error){
			"GetExperiments": func() (any, error) {
				return api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{
					ProjectId: int32(projectID),
				})
			},
			"SearchExperiments": func() (any, error) {
				return api.SearchExperiments(ctx, &apiv1.SearchExperimentsRequest{
					ProjectId: ptrs.Ptr(int32(projectID)),
				})
			},
			"SearchRuns": func() (any, error) {
				return api.SearchRuns(ctx, &apiv1.SearchRunsRequest{
					ProjectId: ptrs.Ptr(int32(projectID)),
				})
			},
			"GetExperiment": func() (any, error) {
				return api.GetExperiment(ctx, &apiv1.GetExperimentRequest{
					ExperimentId: int32(exp.ID),
				})
			},
			"GetGenericTasks": func() (any, error) {
				return api.GetGenericTasks(ctx, &apiv1.GetGenericTasksRequest{
					TaskIds: []string{generic.TaskId},
				})
			},
			"GetCommands": func() (any, error) {
				return api.GetCommands(ctx, &apiv1.GetCommandsRequest{})
			},
			"GetShells": func() (any, error) {
				return api.GetShells(ctx, &apiv1.GetShellsRequest{})
			},
			"GetJobsV2": func() (any, error) {
				return api.GetJobsV2(ctx, &apiv1.GetJobsV2Request{ResourcePool: "default"})
			},
			"GetTasks": func() (any, error) {
				return api.GetTasks(ctx, &apiv1.GetTasksRequest{})
			},
			"TaskLogs": func() (any, error) {
				stream := &mockStream[*apiv1.TaskLogsResponse]{ctx: ctx}
				err := api.TaskLogs(&apiv1.TaskLogsRequest{TaskId: string(trialTask.TaskID)}, stream)
				if len(stream.getData()) > 0 {
					return stream.getData(), err
				}
				return nil, err
			},
		}
	}
	requireNoAnswer := func(t *testing.T, ctx context.Context, code codes.Code) {
		t.Helper()
		for name, read := range reads(ctx) {
			resp, err := read()
			require.Equal(t, code, status.Code(err), "%s: %v", name, err)
			require.Nil(t, resp, name)
		}
	}

	t.Run("policy read error", func(t *testing.T) {
		restore := jobprivacy.FailPolicyReadsForTests(errors.New("the policy table is gone"))
		t.Cleanup(restore)
		requireNoAnswer(t, otherCtx, codes.Unavailable)
		for name, read := range reads(adminCtx) {
			_, err := read()
			require.NoError(t, err, name)
		}
	})

	t.Run("admin predicate error", func(t *testing.T) {
		registerJobPrivacyFailingAuthZ.Do(func() {
			cluster.AuthZProvider.Register("jobprivacy-failing",
				jobPrivacyFailingAuthZ{&cluster.MiscAuthZBasic{}})
		})
		oldAuthZ := config.GetMasterConfig().Security.AuthZ
		config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{
			Type: "jobprivacy-failing", FallbackType: ptrs.Ptr(config.BasicAuthZType),
		}
		t.Cleanup(func() { config.GetMasterConfig().Security.AuthZ = oldAuthZ })
		requireNoAnswer(t, otherCtx, codes.Internal)
		requireNoAnswer(t, ownerCtx, codes.Internal)
	})
}
