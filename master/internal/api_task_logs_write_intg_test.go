//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	authz2 "github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

// logWriteTask is a task that is not a trial, as the master persists it, and the allocation that
// its containers ship their output from.
type logWriteTask struct {
	name         string
	taskID       model.TaskID
	allocationID model.AllocationID
}

// addCommandTaskForLogsTest persists a command, notebook, shell or TensorBoard the way
// Command.registerJobAndTask and Command.persist do: a job that the owner owns, the task, its
// allocation and its command state in a workspace.
func addCommandTaskForLogsTest(
	ctx context.Context, t *testing.T, owner model.User, taskType model.TaskType,
	jobType model.JobType, workspaceID int,
) logWriteTask {
	t.Helper()
	jobID := model.NewJobID()
	require.NoError(t, db.AddJob(&model.Job{JobID: jobID, JobType: jobType, OwnerID: &owner.ID}))
	taskID := model.NewTaskID()
	require.NoError(t, db.AddTask(ctx, &model.Task{
		TaskID: taskID, TaskType: taskType, JobID: &jobID, StartTime: time.Now().UTC(),
		LogVersion: model.CurrentTaskLogVersion,
	}))
	allocationID := model.AllocationID(taskID.String() + ".1")
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		TaskID: taskID, AllocationID: allocationID,
	}))
	spec := tasks.GenericCommandSpec{
		Base: tasks.TaskSpec{Owner: &owner, TaskID: taskID.String()}, TaskType: taskType,
	}
	spec.Metadata.WorkspaceID = model.AccessScopeID(workspaceID)
	_, err := db.Bun().NewInsert().Model(&command.CommandSnapshot{
		TaskID: taskID, RegisteredTime: time.Now().UTC(), AllocationID: allocationID,
		GenericCommandSpec: spec,
	}).Exec(ctx)
	require.NoError(t, err)
	return logWriteTask{name: string(taskType), taskID: taskID, allocationID: allocationID}
}

// addCheckpointGCTaskForLogsTest persists a checkpoint GC task of the experiment in a job the way
// runCheckpointGCTask does: the task, with no command state, and its allocation.
func addCheckpointGCTaskForLogsTest(
	ctx context.Context, t *testing.T, name string, jobID model.JobID, taskID model.TaskID,
) logWriteTask {
	t.Helper()
	require.NoError(t, db.AddTask(ctx, &model.Task{
		TaskID: taskID, TaskType: model.TaskTypeCheckpointGC, JobID: &jobID,
		StartTime: time.Now().UTC(), LogVersion: model.CurrentTaskLogVersion,
	}))
	allocationID := model.AllocationID(taskID.String() + ".1")
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		TaskID: taskID, AllocationID: allocationID,
	}))
	return logWriteTask{name: name, taskID: taskID, allocationID: allocationID}
}

// expGCTaskID is the ID that runCheckpointGCForCheckpoints gives a GC task of an experiment.
func expGCTaskID(expID int) model.TaskID {
	return model.TaskID(fmt.Sprintf("%d.%s", expID, uuid.New()))
}

// postTaskLogsGRPC posts a line to the task's logs through PostTaskLogs, as ship_logs.py does
// through the gateway, with whatever credentials ctx carries.
func postTaskLogsGRPC(ctx context.Context, api *apiServer, taskID model.TaskID, line string) error {
	_, err := api.PostTaskLogs(ctx, &apiv1.PostTaskLogsRequest{Logs: []*taskv1.TaskLog{{
		TaskId: string(taskID), Timestamp: timestamppb.Now(), Log: line + "\n",
		Source: ptrs.Ptr("task"), Stdtype: ptrs.Ptr("stdout"),
	}}})
	return err
}

// readTaskLogs returns the number of lines of the task's logs that the user of ctx reads.
func readTaskLogs(ctx context.Context, t *testing.T, api *apiServer, taskID model.TaskID) int {
	t.Helper()
	stream := &mockStream[*apiv1.TaskLogsResponse]{ctx: ctx}
	require.NoError(t, api.TaskLogs(&apiv1.TaskLogsRequest{TaskId: string(taskID)}, stream))
	return len(stream.getData())
}

// TestTaskLogWritesNeedTaskControl checks the write rule for the logs of tasks that are not trials
// under basic authorization: their owner, an admin and their own containers may write, on both
// routes, and another user, who may see them and read their logs, may not.
func TestTaskLogWritesNeedTaskControl(t *testing.T) {
	api, admin, ctx := setupAPITest(t, nil)
	srv := taskLogsServer(t, api)
	owner := db.RequireMockUser(t, api.m.db)
	other := db.RequireMockUser(t, api.m.db)
	ownerToken := sessionToken(t, owner)
	otherToken := sessionToken(t, other)
	adminToken := sessionToken(t, admin)
	ownerCtx, otherCtx := sessionContext(t, owner), sessionContext(t, other)

	const workspaceID = 1
	exp := createTestExpWithProjectID(t, api, owner, 1)
	genericID := addGenericTaskForAuthZTest(ctx, t, owner, workspaceID, nil, model.TaskStateActive)
	taskList := []logWriteTask{
		addCommandTaskForLogsTest(ctx, t, owner, model.TaskTypeCommand, model.JobTypeCommand,
			workspaceID),
		addCommandTaskForLogsTest(ctx, t, owner, model.TaskTypeNotebook, model.JobTypeNotebook,
			workspaceID),
		addCommandTaskForLogsTest(ctx, t, owner, model.TaskTypeShell, model.JobTypeShell,
			workspaceID),
		addCommandTaskForLogsTest(ctx, t, owner, model.TaskTypeTensorboard,
			model.JobTypeTensorboard, workspaceID),
		{
			name: string(model.TaskTypeGeneric), taskID: genericID,
			allocationID: model.AllocationID(genericID.String() + ".0"),
		},
		addCheckpointGCTaskForLogsTest(ctx, t, string(model.TaskTypeCheckpointGC), exp.JobID,
			expGCTaskID(exp.ID)),
	}
	// The containers of another user's task authenticate as that user.
	otherTask := addCommandTaskForLogsTest(ctx, t, other, model.TaskTypeCommand,
		model.JobTypeCommand, workspaceID)
	otherTaskSession := allocationSessionCtx(t, otherTask.allocationID, other)

	for _, task := range taskList {
		t.Run(task.name, func(t *testing.T) {
			// Another non-admin user can see the task but may not write to its logs, on either
			// route, and neither may the containers of a task of theirs.
			forged := jsonBody(t, shippedLogs(t, task.taskID, "forged"))
			require.Equal(t, http.StatusForbidden, postTaskLogsAs(t, srv, otherToken, forged))
			requireCode(t, codes.PermissionDenied,
				postTaskLogsGRPC(otherCtx, api, task.taskID, "forged"))
			requireCode(t, codes.PermissionDenied,
				postTaskLogsGRPC(otherTaskSession, api, task.taskID, "forged"))
			require.Zero(t, taskLogRows(t, task.taskID))

			// The owner and an admin may write, on both routes.
			require.Equal(t, http.StatusOK, postTaskLogsAs(t, srv, ownerToken,
				jsonBody(t, shippedLogs(t, task.taskID, "owner"))))
			require.NoError(t, postTaskLogsGRPC(ownerCtx, api, task.taskID, "owner"))
			require.Equal(t, http.StatusOK, postTaskLogsAs(t, srv, adminToken,
				jsonBody(t, shippedLogs(t, task.taskID, "admin"))))
			require.NoError(t, postTaskLogsGRPC(ctx, api, task.taskID, "admin"))
			// So may the task's own containers, with its allocation session, which acts as the
			// user the task runs as, its owner.
			ownSession := allocationSessionCtx(t, task.allocationID, owner)
			require.NoError(t, postTaskLogsGRPC(ownSession, api, task.taskID, "shipped"))
			require.Equal(t, 5, taskLogRows(t, task.taskID))

			// The other user still sees the task and reads its logs.
			_, err := api.GetTask(otherCtx, &apiv1.GetTaskRequest{TaskId: task.taskID.String()})
			require.NoError(t, err)
			require.Equal(t, 5, readTaskLogs(otherCtx, t, api, task.taskID))
		})
	}

	// A task whose owner is unknown is refused, but for its own containers.
	ownerless := mockNotebookWithWorkspaceID(ctx, t, workspaceID)
	require.Equal(t, http.StatusForbidden, postTaskLogsAs(t, srv, adminToken,
		jsonBody(t, shippedLogs(t, ownerless, "forged"))))
	requireCode(t, codes.PermissionDenied, postTaskLogsGRPC(ctx, api, ownerless, "forged"))
	require.Zero(t, taskLogRows(t, ownerless))
}

// TestCheckpointGCShipsItsOwnLogs checks that a checkpoint GC task's containers, which ship their
// output with the GC task's allocation session, may write its logs whichever user the GC runs as.
// That user is not always the owner of the GC task's job. Before PR #47, deleting an experiment or
// its TensorBoard files ran the GC as the user who asked, in the experiment's job; that user may
// edit or delete the experiment, but under RBAC need not own it or be an admin. With PR #47,
// removing another user's checkpoint files runs the GC as the experiment's owner, in a job of its
// own that the user who asked, such as an admin, owns.
func TestCheckpointGCShipsItsOwnLogs(t *testing.T) {
	api, admin, ctx := setupAPITest(t, nil)
	owner := db.RequireMockUser(t, api.m.db)
	deleter := db.RequireMockUser(t, api.m.db)
	exp := createTestExpWithProjectID(t, api, owner, 1)

	// GC tasks in the experiment's job, run as its owner, an admin or another user.
	for _, runAs := range []model.User{owner, admin, deleter} {
		gc := addCheckpointGCTaskForLogsTest(ctx, t, "experiment job", exp.JobID,
			expGCTaskID(exp.ID))
		session := allocationSessionCtx(t, gc.allocationID, runAs)
		require.NoError(t, postTaskLogsGRPC(session, api, gc.taskID, "deleting"))
		// The other user's own session is not the task's.
		requireCode(t, codes.PermissionDenied,
			postTaskLogsGRPC(sessionContext(t, deleter), api, gc.taskID, "forged"))
		require.Equal(t, 1, taskLogRows(t, gc.taskID))
	}

	// A GC task in a job that an admin who removed the files owns, run as the experiment's owner.
	jobID := model.NewJobID()
	require.NoError(t, db.AddJob(&model.Job{
		JobID: jobID, JobType: model.JobTypeCheckpointGC, OwnerID: &admin.ID,
	}))
	removeFiles := addCheckpointGCTaskForLogsTest(ctx, t, "own job", jobID, model.NewTaskID())
	session := allocationSessionCtx(t, removeFiles.allocationID, owner)
	require.NoError(t, postTaskLogsGRPC(session, api, removeFiles.taskID, "removing"))
	// The experiment's owner, with their own session, is not the owner of that task.
	requireCode(t, codes.PermissionDenied,
		postTaskLogsGRPC(sessionContext(t, owner), api, removeFiles.taskID, "forged"))
	require.Equal(t, 1, taskLogRows(t, removeFiles.taskID))
}

// TestTaskLogWritesNTSCAuthZ checks which authz methods the write rule asks, as under RBAC.
func TestTaskLogWritesNTSCAuthZ(t *testing.T) {
	// The routes look up CanEditExperiment whatever the task type, so the experiment mock must be
	// registered as well.
	_, _, _, _, _ = setupExpAuthTest(t, nil) //nolint: dogsled
	api, authZNSC, _, adminCtx := setupNTSCAuthzTest(t)
	srv := taskLogsServer(t, api)
	owner := db.RequireMockUser(t, api.m.db)
	curUser := db.RequireMockUser(t, api.m.db)
	token := sessionToken(t, curUser)
	ctx := sessionContext(t, curUser)
	asCurUser := mock.MatchedBy(func(u model.User) bool { return u.ID == curUser.ID })
	asOwner := mock.MatchedBy(func(u model.User) bool { return u.ID == owner.ID })

	const workspaceID = -120
	ws := model.AccessScopeID(workspaceID)
	nb := addCommandTaskForLogsTest(adminCtx, t, owner, model.TaskTypeNotebook,
		model.JobTypeNotebook, workspaceID)
	body := jsonBody(t, shippedLogs(t, nb.taskID, "line"))

	// A user who cannot see the task is told that it does not exist.
	authZNSC.On("CanGetNSC", mock.Anything, asCurUser, ws).
		Return(authz2.PermissionDeniedError{}).Twice()
	require.Equal(t, http.StatusNotFound, postTaskLogsAs(t, srv, token, body))
	requireCode(t, codes.NotFound, postTaskLogsGRPC(ctx, api, nb.taskID, "line"))

	// A user who can see it but may not control it, without UPDATE_NSC under RBAC, is forbidden.
	authZNSC.On("CanGetNSC", mock.Anything, asCurUser, ws).Return(nil).Twice()
	authZNSC.On("CanControlGenericTask", mock.Anything, asCurUser, ws, ptrs.Ptr(owner.ID)).
		Return(authz2.PermissionDeniedError{}).Twice()
	require.Equal(t, http.StatusForbidden, postTaskLogsAs(t, srv, token, body))
	requireCode(t, codes.PermissionDenied, postTaskLogsGRPC(ctx, api, nb.taskID, "line"))
	require.Zero(t, taskLogRows(t, nb.taskID))

	// A user who may control it may write, whoever owns it.
	authZNSC.On("CanGetNSC", mock.Anything, asCurUser, ws).Return(nil).Twice()
	authZNSC.On("CanControlGenericTask", mock.Anything, asCurUser, ws, ptrs.Ptr(owner.ID)).
		Return(nil).Twice()
	require.Equal(t, http.StatusOK, postTaskLogsAs(t, srv, token, body))
	require.NoError(t, postTaskLogsGRPC(ctx, api, nb.taskID, "line"))
	require.Equal(t, 2, taskLogRows(t, nb.taskID))

	// The task's own containers need only see it.
	authZNSC.On("CanGetNSC", mock.Anything, asOwner, ws).Return(nil).Once()
	ownSession := allocationSessionCtx(t, nb.allocationID, owner)
	require.NoError(t, postTaskLogsGRPC(ownSession, api, nb.taskID, "shipped"))
	require.Equal(t, 3, taskLogRows(t, nb.taskID))

	// Reading the logs takes only the view check, as before.
	authZNSC.On("CanGetNSC", mock.Anything, asCurUser, ws).Return(nil).Once()
	require.Equal(t, 3, readTaskLogs(ctx, t, api, nb.taskID))

	// A checkpoint GC task asks no NSC permission: its owner or an admin may write, in every authz
	// mode, and no one else.
	exp := createTestExpWithProjectID(t, api, owner, 1)
	gc := addCheckpointGCTaskForLogsTest(adminCtx, t, "gc", exp.JobID, expGCTaskID(exp.ID))
	require.Equal(t, http.StatusForbidden, postTaskLogsAs(t, srv, token,
		jsonBody(t, shippedLogs(t, gc.taskID, "forged"))))
	requireCode(t, codes.PermissionDenied, postTaskLogsGRPC(ctx, api, gc.taskID, "forged"))
	require.Zero(t, taskLogRows(t, gc.taskID))
	require.NoError(t, postTaskLogsGRPC(sessionContext(t, owner), api, gc.taskID, "owner"))
	require.NoError(t, postTaskLogsGRPC(adminCtx, api, gc.taskID, "admin"))
	require.Equal(t, 2, taskLogRows(t, gc.taskID))

	authZNSC.AssertExpectations(t)
}

func TestPostTaskLogsRefusesNullLog(t *testing.T) {
	api, curUser, ctx := setupAPITest(t, nil)
	_, task := createTestTrial(t, api, curUser)

	for _, logs := range [][]*taskv1.TaskLog{
		{nil},
		{{TaskId: string(task.TaskID), Log: "line"}, nil},
	} {
		_, err := api.PostTaskLogs(ctx, &apiv1.PostTaskLogsRequest{Logs: logs})
		requireCode(t, codes.InvalidArgument, err)
		require.ErrorContains(t, err, "logs must not be null")
	}
	require.Zero(t, taskLogRows(t, task.TaskID))
}
