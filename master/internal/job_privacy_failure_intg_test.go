//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/logger"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

// jobPrivacyEditorAuthZ is an experiment authorization in which every user may edit every
// experiment, as a workspace editor who is not the owner may under RBAC.
type jobPrivacyEditorAuthZ struct {
	*experiment.ExperimentAuthZBasic
}

// CanEditExperiment allows every user.
func (jobPrivacyEditorAuthZ) CanEditExperiment(
	context.Context, model.User, *model.Experiment,
) error {
	return nil
}

// CanEditExperimentsMetadata allows every user.
func (jobPrivacyEditorAuthZ) CanEditExperimentsMetadata(
	context.Context, model.User, *model.Experiment,
) error {
	return nil
}

var registerJobPrivacyEditorAuthZ sync.Once

// useJobPrivacyEditorAuthZ makes every user an editor of every experiment until the test ends.
// Everything else, the admin predicate included, stays basic.
func useJobPrivacyEditorAuthZ(t *testing.T) {
	registerJobPrivacyEditorAuthZ.Do(func() {
		experiment.AuthZProvider.Register("jobprivacy-editor",
			jobPrivacyEditorAuthZ{&experiment.ExperimentAuthZBasic{}})
	})
	oldAuthZ := config.GetMasterConfig().Security.AuthZ
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{
		Type: "jobprivacy-editor", FallbackType: ptrs.Ptr(config.BasicAuthZType),
	}
	t.Cleanup(func() { config.GetMasterConfig().Security.AuthZ = oldAuthZ })
}

// jobPrivacyNSCControlAuthZ is a command and shell authorization in which every user may control
// every command and shell, as a workspace editor who is not the owner may under RBAC.
type jobPrivacyNSCControlAuthZ struct {
	*command.NSCAuthZBasic
}

// CanControlGenericTask allows every user.
func (jobPrivacyNSCControlAuthZ) CanControlGenericTask(
	context.Context, model.User, model.AccessScopeID, *model.UserID,
) error {
	return nil
}

var registerJobPrivacyNSCControlAuthZ sync.Once

// useJobPrivacyNSCControlAuthZ lets every user kill every command and shell and set its priority
// until the test ends. Everything else, the admin predicate included, stays basic.
func useJobPrivacyNSCControlAuthZ(t *testing.T) {
	registerJobPrivacyNSCControlAuthZ.Do(func() {
		command.AuthZProvider.Register("jobprivacy-nsc-control",
			jobPrivacyNSCControlAuthZ{&command.NSCAuthZBasic{}})
	})
	oldAuthZ := config.GetMasterConfig().Security.AuthZ
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{
		Type: "jobprivacy-nsc-control", FallbackType: ptrs.Ptr(config.BasicAuthZType),
	}
	t.Cleanup(func() { config.GetMasterConfig().Security.AuthZ = oldAuthZ })
}

// TestJobPrivacyTrialUpsert checks that a PUT of an existing external trial, which returns the
// stored trial, redacts its log policy match as GetTrial does: an editor who may not read the
// logs gets no match from either, while the owner and an admin get it from both.
func TestJobPrivacyTrialUpsert(t *testing.T) {
	var aux, compute string
	api, admin, _ := setupAPITest(t, nil, mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute)))
	lockJobPrivacyPolicy(t)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)
	useJobPrivacyEditorAuthZ(t)
	owner := addGCTestOwner(t)
	editor := addGCTestOwner(t)
	ownerCtx := userContext(t, owner)

	const policy = "secret-upsert-policy"
	exp, err := api.PutExperiment(ownerCtx, &apiv1.PutExperimentRequest{
		CreateExperimentRequest: &apiv1.CreateExperimentRequest{
			Config: accessTestExperimentConfig(compute,
				"log_policies:\n  - name: "+policy+"\n    pattern: SECRET-UPSERT-MARKER\n"),
			Unmanaged: ptrs.Ptr(true), ProjectId: 1,
		},
		ExternalExperimentId: uuid.NewString(),
	})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, exp.Experiment.JobId, true, false, true)

	putReq := &apiv1.PutTrialRequest{
		CreateTrialRequest: &apiv1.CreateTrialRequest{
			ExperimentId: exp.Experiment.Id, Unmanaged: true,
		},
		ExternalTrialId: uuid.NewString(),
	}
	created, err := api.PutTrial(ownerCtx, putReq)
	require.NoError(t, err)
	require.Nil(t, created.Trial.LogPolicyMatched)
	_, err = api.PostTaskLogs(ownerCtx, &apiv1.PostTaskLogsRequest{
		Logs: []*taskv1.TaskLog{{
			TaskId: created.Trial.TaskIds[0], AgentId: ptrs.Ptr("a1"),
			Log: "a line with SECRET-UPSERT-MARKER in it",
		}},
	})
	require.NoError(t, err)

	for name, vw := range map[string]struct {
		user model.User
		full bool
	}{
		"owner":  {owner, true},
		"admin":  {admin, true},
		"editor": {editor, false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := userContext(t, vw.user)
			want := (*string)(nil)
			if vw.full {
				want = ptrs.Ptr(policy)
			}
			got, err := api.GetTrial(ctx, &apiv1.GetTrialRequest{TrialId: created.Trial.Id})
			require.NoError(t, err)
			require.Equal(t, want, got.Trial.LogPolicyMatched, "GetTrial")
			put, err := api.PutTrial(ctx, putReq)
			require.NoError(t, err)
			require.Equal(t, created.Trial.Id, put.Trial.Id, "the PUT finds the stored trial")
			require.Equal(t, want, put.Trial.LogPolicyMatched, "PutTrial")
		})
	}
}

// TestJobPrivacyUnmanagedTrialTasks checks that the tasks of unmanaged trials, created by
// CreateTrial and PutTrial without a job of their own, take the capabilities of their experiment's
// job, hidden and then shared, from jobprivacy.ForTasks and taskCaps.
func TestJobPrivacyUnmanagedTrialTasks(t *testing.T) {
	var aux, compute string
	api, admin, _ := setupAPITest(t, nil, mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute)))
	lockJobPrivacyPolicy(t)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)
	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	ownerCtx := userContext(t, owner)
	ctx := context.Background()

	exp, err := api.PutExperiment(ownerCtx, &apiv1.PutExperimentRequest{
		CreateExperimentRequest: &apiv1.CreateExperimentRequest{
			Config: accessTestExperimentConfig(compute), Unmanaged: ptrs.Ptr(true), ProjectId: 1,
		},
		ExternalExperimentId: uuid.NewString(),
	})
	require.NoError(t, err)
	created, err := api.CreateTrial(ownerCtx, &apiv1.CreateTrialRequest{
		ExperimentId: exp.Experiment.Id, Unmanaged: true,
	})
	require.NoError(t, err)
	put, err := api.PutTrial(ownerCtx, &apiv1.PutTrialRequest{
		CreateTrialRequest: &apiv1.CreateTrialRequest{
			ExperimentId: exp.Experiment.Id, Unmanaged: true,
		},
		ExternalTrialId: uuid.NewString(),
	})
	require.NoError(t, err)
	taskIDs := []model.TaskID{
		model.TaskID(created.Trial.TaskIds[0]), model.TaskID(put.Trial.TaskIds[0]),
	}
	for _, id := range taskIDs {
		task, err := db.TaskByID(ctx, id)
		require.NoError(t, err)
		require.Nil(t, task.JobID, "the task of an unmanaged trial has no job of its own")
	}

	requireCaps := func(u model.User, want jobprivacy.Caps, msg string) {
		t.Helper()
		v := jobprivacy.Viewer{ID: u.ID}
		caps, err := jobprivacy.ForTasks(ctx, v, taskIDs)
		require.NoError(t, err, msg)
		for _, id := range taskIDs {
			require.Equal(t, want, caps[id], "ForTasks: %s", msg)
			got, err := taskCaps(ctx, v, id)
			require.NoError(t, err, msg)
			require.Equal(t, want, got, "taskCaps: %s", msg)
		}
	}
	jobID := exp.Experiment.JobId
	shareJob(ownerCtx, t, api, jobID, false, false, false)
	requireCaps(other, jobprivacy.Caps{}, "hidden")
	requireCaps(owner, jobprivacy.AllCaps, "hidden, the owner")
	shareJob(ownerCtx, t, api, jobID, true, true, true)
	requireCaps(other, jobprivacy.Caps{ViewTitle: true, ViewLogs: true, ViewConfig: true}, "shared")
	shareJob(ownerCtx, t, api, jobID, true, false, true)
	requireCaps(other, jobprivacy.Caps{ViewTitle: true, ViewConfig: true}, "logs hidden")
}

// TestJobPrivacyLabelEdits checks that an editor who may not read an experiment's title can add
// and remove one label: every response, those that change nothing included, carries no labels,
// and the stored labels keep the others, which the editor never saw.
func TestJobPrivacyLabelEdits(t *testing.T) {
	api, _, _ := setupAPITest(t, nil)
	lockJobPrivacyPolicy(t)
	useJobPrivacyEditorAuthZ(t)
	owner := addGCTestOwner(t)
	editor := addGCTestOwner(t)
	ownerCtx, editorCtx := userContext(t, owner), userContext(t, editor)

	keepA, keepB, label := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exp := createTestExpWithProjectID(t, api, owner, 1, keepA, keepB)
	shareJob(ownerCtx, t, api, exp.JobID.String(), false, true, true)
	expID := int32(exp.ID)

	stored := func() []string {
		t.Helper()
		var labels []string
		require.NoError(t, db.Bun().NewRaw(
			`SELECT jsonb_array_elements_text(config->'labels') FROM experiments WHERE id = ?`,
			expID).Scan(context.Background(), &labels))
		return labels
	}

	put := func(msg string) {
		t.Helper()
		resp, err := api.PutExperimentLabel(editorCtx,
			&apiv1.PutExperimentLabelRequest{ExperimentId: expID, Label: label})
		require.NoError(t, err, msg)
		require.Empty(t, resp.Labels, msg)
		require.ElementsMatch(t, []string{keepA, keepB, label}, stored(), msg)
	}
	del := func(msg string) {
		t.Helper()
		resp, err := api.DeleteExperimentLabel(editorCtx,
			&apiv1.DeleteExperimentLabelRequest{ExperimentId: expID, Label: label})
		require.NoError(t, err, msg)
		require.Empty(t, resp.Labels, msg)
		require.ElementsMatch(t, []string{keepA, keepB}, stored(), msg)
	}
	put("add")
	put("add a label that exists")
	del("remove")
	del("remove a label that is absent")

	resp, err := api.PutExperimentLabel(ownerCtx,
		&apiv1.PutExperimentLabelRequest{ExperimentId: expID, Label: label})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{keepA, keepB, label}, resp.Labels, "the owner reads them")
}

// exitAllocationService starts allocations as its AllocationService does and keeps their exit
// callbacks, so that a test can end an allocation with an exit of its choice.
type exitAllocationService struct {
	task.AllocationService
	mu     sync.Mutex
	onExit map[model.TaskID]func(*task.AllocationExited)
}

func (s *exitAllocationService) StartAllocation(
	logCtx logger.Context, req sproto.AllocateRequest, idb db.DB, r rm.ResourceManager,
	spec tasks.TaskSpecifier, onExit func(*task.AllocationExited),
) error {
	s.mu.Lock()
	s.onExit[req.TaskID] = onExit
	s.mu.Unlock()
	return s.AllocationService.StartAllocation(logCtx, req, idb, r, spec, onExit)
}

// exit ends the task's allocation with err, as the allocation does when it exits.
func (s *exitAllocationService) exit(t *testing.T, taskID string, err error) {
	t.Helper()
	s.mu.Lock()
	onExit := s.onExit[model.TaskID(taskID)]
	s.mu.Unlock()
	require.NotNil(t, onExit, taskID)
	onExit(&task.AllocationExited{Err: err, FinalState: task.AllocationState{
		State: model.AllocationStateTerminated,
	}})
}

// failureMarker is the text of every free-text failure reason of TestJobPrivacyFailureReasons.
const failureMarker = "error pulling image: registry.example/secret-failure-image:1"

// containsFailureMarker reports whether a response carries failureMarker.
func containsFailureMarker(t *testing.T, resp any) bool {
	t.Helper()
	var b []byte
	var err error
	if m, ok := resp.(proto.Message); ok {
		b, err = protojson.Marshal(m)
	} else {
		b, err = json.Marshal(resp)
	}
	require.NoError(t, err)
	return strings.Contains(string(b), failureMarker)
}

// TestJobPrivacyFailureReasons checks that the free-text failure reasons of allocations, resources,
// commands and shells follow the logs: a user who may read the title but not the logs gets them
// empty from every route that carries them, with the state, failure type and exit code kept,
// while the owner and an admin get them.
func TestJobPrivacyFailureReasons(t *testing.T) {
	var aux, compute string
	var summaries map[model.AllocationID]sproto.AllocationSummary
	mockRM := mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute))
	mockRM.On("GetAllocationSummaries", mock.Anything).Return(
		func() map[model.AllocationID]sproto.AllocationSummary { return summaries }, nil)
	api, admin, adminCtx := setupAPITest(t, nil, mockRM)
	lockJobPrivacyPolicy(t)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)
	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	ownerCtx := userContext(t, owner)

	// A trial task with the experiment's job and one without a job, which takes the experiment's.
	exp := createTestExp(t, api, owner)
	shareJob(ownerCtx, t, api, exp.JobID.String(), true, false, true)
	_, trialTask := db.RequireMockTrial(t, db.SingleDB(), exp)
	_, err = db.Bun().NewRaw(`UPDATE tasks SET job_id = ? WHERE task_id = ?`,
		exp.JobID, trialTask.TaskID).Exec(adminCtx)
	require.NoError(t, err)
	_, jobless := db.RequireMockTrial(t, db.SingleDB(), exp)
	_, err = db.Bun().NewRaw(`UPDATE tasks SET job_id = NULL WHERE task_id = ?`,
		jobless.TaskID).Exec(adminCtx)
	require.NoError(t, err)

	services := &exitAllocationService{
		AllocationService: task.DefaultService,
		onExit:            map[model.TaskID]func(*task.AllocationExited){},
	}
	oldService := task.DefaultService
	task.DefaultService = services
	t.Cleanup(func() { task.DefaultService = oldService })
	cmd, err := api.LaunchCommand(ownerCtx, &apiv1.LaunchCommandRequest{})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, cmd.Command.JobId, true, false, true)
	shell, err := api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, shell.Shell.JobId, true, false, true)
	services.exit(t, cmd.Command.Id, errors.New(failureMarker))
	services.exit(t, shell.Shell.Id, errors.New(failureMarker))

	// An exited allocation of each task, with the marker as its exit reason.
	allocationIDs := map[string]string{}
	for _, taskID := range []string{
		string(trialTask.TaskID), string(jobless.TaskID), cmd.Command.Id,
	} {
		allocationID := taskID + ".privacy"
		now := time.Now().UTC()
		require.NoError(t, db.AddAllocation(adminCtx, &model.Allocation{
			AllocationID: model.AllocationID(allocationID), TaskID: model.TaskID(taskID),
			Slots: 1, ResourcePool: "default", StartTime: &now, EndTime: &now,
			State: ptrs.Ptr(model.AllocationStateTerminated), Ports: map[string]int{},
			ExitReason: ptrs.Ptr(failureMarker), StatusCode: ptrs.Ptr(int32(1)),
		}))
		allocationIDs[taskID] = allocationID
	}

	summaryID := model.AllocationID(allocationIDs[string(trialTask.TaskID)])
	summaries = map[model.AllocationID]sproto.AllocationSummary{
		summaryID: {
			TaskID: trialTask.TaskID, AllocationID: summaryID, Name: "shared-allocation",
			RegisteredTime: time.Now(), ResourcePool: "default",
			Resources: []sproto.ResourcesSummary{{
				ResourcesID: "privacy-resources", AllocationID: summaryID,
				Exited: &sproto.ResourcesStopped{Failure: &sproto.ResourcesFailedError{
					FailureType: sproto.ResourcesFailed, ErrMsg: failureMarker,
					ExitCode: ptrs.Ptr(sproto.ExitCode(1)),
				}},
			}},
		},
	}

	for name, vw := range map[string]struct {
		user model.User
		full bool
	}{
		"owner": {owner, true},
		"admin": {admin, true},
		"other": {other, false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := userContext(t, vw.user)
			requireMarker := func(route string, resp any) {
				t.Helper()
				require.Equal(t, vw.full, containsFailureMarker(t, resp), route)
			}

			for taskID, allocationID := range allocationIDs {
				got, err := api.GetTask(ctx, &apiv1.GetTaskRequest{TaskId: taskID})
				require.NoError(t, err, taskID)
				requireMarker("GetTask "+taskID, got)
				var a *taskv1.Allocation
				for _, ta := range got.Task.Allocations {
					if ta.AllocationId == allocationID {
						a = ta
					}
				}
				require.NotNil(t, a, allocationID)
				require.Equal(t, int32(1), a.GetStatusCode(), taskID)
				require.NotEqual(t, taskv1.State_STATE_UNSPECIFIED, a.State, taskID)
				if !vw.full {
					require.Equal(t, "", a.GetExitReason(), taskID)
				}

				alloc, err := api.GetAllocation(ctx, &apiv1.GetAllocationRequest{
					AllocationId: allocationID,
				})
				require.NoError(t, err, allocationID)
				requireMarker("GetAllocation "+allocationID, alloc)
				require.Equal(t, int32(1), alloc.Allocation.GetStatusCode(), allocationID)
			}

			tasksResp, err := api.GetTasks(ctx, &apiv1.GetTasksRequest{})
			require.NoError(t, err)
			requireMarker("GetTasks", tasksResp)
			failure := tasksResp.AllocationIdToSummary[string(summaryID)].
				Resources[0].Exited.Failure
			require.Equal(t, taskv1.FailureType_FAILURE_TYPE_RESOURCES_FAILED, failure.FailureType)
			require.Equal(t, int32(1), failure.GetExitCode())
			require.Equal(t, "shared-allocation",
				tasksResp.AllocationIdToSummary[string(summaryID)].Name)

			tasksCtx, _ := echoContextAs(vw.user, "/tasks", nil)
			echoTasks, err := api.m.getTasks(tasksCtx)
			require.NoError(t, err)
			requireMarker("GET /tasks", echoTasks)
			echoFailure := echoTasks.(map[model.AllocationID]sproto.AllocationSummary)[summaryID].
				Resources[0].Exited.Failure
			require.Equal(t, sproto.ResourcesFailed, echoFailure.FailureType)
			require.Equal(t, sproto.ExitCode(1), *echoFailure.ExitCode)

			cmds, err := api.GetCommands(ctx, &apiv1.GetCommandsRequest{})
			require.NoError(t, err)
			requireMarker("GetCommands", cmds)
			getCmd, err := api.GetCommand(ctx, &apiv1.GetCommandRequest{CommandId: cmd.Command.Id})
			require.NoError(t, err)
			requireMarker("GetCommand", getCmd)
			require.NotEqual(t, taskv1.State_STATE_UNSPECIFIED, getCmd.Command.State)
			shells, err := api.GetShells(ctx, &apiv1.GetShellsRequest{})
			require.NoError(t, err)
			requireMarker("GetShells", shells)
			getShell, err := api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: shell.Shell.Id})
			require.NoError(t, err)
			requireMarker("GetShell", getShell)

			// Under basic authorization only the owner and admins control a command or shell;
			// the other user controls them as a workspace editor may under RBAC.
			if !vw.full {
				useJobPrivacyNSCControlAuthZ(t)
			}
			prio, err := api.SetCommandPriority(ctx, &apiv1.SetCommandPriorityRequest{
				CommandId: cmd.Command.Id, Priority: 10,
			})
			require.NoError(t, err)
			requireMarker("SetCommandPriority", prio)
			require.Equal(t, getCmd.Command.Capabilities, prio.Command.Capabilities)
			shellPrio, err := api.SetShellPriority(ctx, &apiv1.SetShellPriorityRequest{
				ShellId: shell.Shell.Id, Priority: 10,
			})
			require.NoError(t, err)
			requireMarker("SetShellPriority", shellPrio)
			require.Equal(t, getShell.Shell.Capabilities, shellPrio.Shell.Capabilities)
			killed, err := api.KillCommand(ctx, &apiv1.KillCommandRequest{CommandId: cmd.Command.Id})
			require.NoError(t, err)
			requireMarker("KillCommand", killed)
			require.Equal(t, getCmd.Command.Capabilities, killed.Command.Capabilities)
			killedShell, err := api.KillShell(ctx, &apiv1.KillShellRequest{ShellId: shell.Shell.Id})
			require.NoError(t, err)
			requireMarker("KillShell", killedShell)
			require.Equal(t, getShell.Shell.Capabilities, killedShell.Shell.Capabilities)
		})
	}

	// The redaction never reaches the resource manager's summaries.
	require.Equal(t, failureMarker, summaries[summaryID].Resources[0].Exited.Failure.ErrMsg)
}

// TestJobPrivacyHTTPUnavailable checks that the HTTP routes answer a policy read error as the gRPC
// routes do: 503 Service Unavailable.
func TestJobPrivacyHTTPUnavailable(t *testing.T) {
	var aux, compute string
	var summaries map[model.AllocationID]sproto.AllocationSummary
	mockRM := mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute))
	mockRM.On("GetAllocationSummaries", mock.Anything).Return(
		func() map[model.AllocationID]sproto.AllocationSummary { return summaries }, nil)
	api, admin, adminCtx := setupAPITest(t, nil, mockRM)
	lockJobPrivacyPolicy(t)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)
	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	exp, _ := createGCTestExperiment(adminCtx, t, api, owner)
	_, trialTask := db.RequireMockTrial(t, db.SingleDB(), exp)
	_, err := db.Bun().NewRaw(`UPDATE tasks SET job_id = ? WHERE task_id = ?`,
		exp.JobID, trialTask.TaskID).Exec(adminCtx)
	require.NoError(t, err)
	summaries = map[model.AllocationID]sproto.AllocationSummary{
		model.AllocationID(trialTask.TaskID + ".1"): {
			TaskID: trialTask.TaskID, AllocationID: model.AllocationID(trialTask.TaskID + ".1"),
			RegisteredTime: time.Now(), ResourcePool: "default",
		},
	}

	restore := jobprivacy.FailPolicyReadsForTests(errors.New("the policy table is gone"))
	t.Cleanup(restore)
	requireUnavailable := func(route string, err error) {
		t.Helper()
		var httpErr *echo.HTTPError
		require.ErrorAs(t, err, &httpErr, route)
		require.Equal(t, http.StatusServiceUnavailable, httpErr.Code, route)
		require.Contains(t, fmt.Sprint(httpErr.Message), "try again", route)
	}
	expParam := map[string]string{"experiment_id": strconv.Itoa(exp.ID)}

	tasksCtx, _ := echoContextAs(other, "/tasks", nil)
	_, err = api.m.getTasks(tasksCtx)
	requireUnavailable("GET /tasks", err)
	previewCtx, _ := echoContextAs(other, fmt.Sprintf("/experiments/%d/preview_gc?"+
		"save_experiment_best=0&save_trial_best=0&save_trial_latest=0", exp.ID), expParam)
	_, err = api.m.getExperimentCheckpointsToGC(previewCtx)
	requireUnavailable("GET /experiments/:experiment_id/preview_gc", err)
	defCtx, _ := echoContextAs(other, fmt.Sprintf("/experiments/%d/model_def", exp.ID), expParam)
	requireUnavailable("GET /experiments/:experiment_id/model_def",
		api.m.getExperimentModelDefinition(defCtx))
}
