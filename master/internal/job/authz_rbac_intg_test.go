//go:build integration
// +build integration

package job

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rbac"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/rbacv1"
)

func TestMain(m *testing.M) {
	pgDB, _, err := db.ResolveTestPostgres()
	if err != nil {
		log.Panicln(err)
	}

	err = db.MigrateTestPostgres(pgDB, "file://../../static/migrations", "up")
	if err != nil {
		log.Panicln(err)
	}

	err = etc.SetRootPath("../../static/srv")
	if err != nil {
		log.Panicln(err)
	}

	os.Exit(m.Run())
}

// useRBACForTest makes every authz provider the RBAC one for the rest of the test.
func useRBACForTest(t *testing.T) {
	t.Helper()
	previous := config.GetMasterConfig().Security.AuthZ
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: "rbac"}
	t.Cleanup(func() { config.GetMasterConfig().Security.AuthZ = previous })
}

// assignRoleForTest gives u the role named roleName in the workspace, or in every workspace when
// workspaceID is nil.
func assignRoleForTest(t *testing.T, u model.User, roleName string, workspaceID *int32) {
	t.Helper()
	ctx := context.Background()
	var roleID int32
	require.NoError(t, db.Bun().NewRaw("SELECT id FROM roles WHERE role_name = ?", roleName).
		Scan(ctx, &roleID))
	require.NoError(t, rbac.AddRoleAssignments(ctx, nil, []*rbacv1.UserRoleAssignment{{
		UserId: int32(u.ID),
		RoleAssignment: &rbacv1.RoleAssignment{
			Role: &rbacv1.Role{RoleId: roleID}, ScopeWorkspaceId: workspaceID,
		},
	}}))
}

// addTaskJobForTest persists a job of jobType owned by owner, run by a task whose command record
// places it in the workspace, as the command and generic task APIs store them.
func addTaskJobForTest(
	t *testing.T, pgDB *db.PgDB, owner model.User, jobType model.JobType, workspaceID int,
) model.JobID {
	t.Helper()
	ctx := context.Background()
	jobID := model.NewJobID()
	require.NoError(t, db.AddJob(&model.Job{JobID: jobID, JobType: jobType, OwnerID: &owner.ID}))
	taskType := map[model.JobType]model.TaskType{
		model.JobTypeShell:   model.TaskTypeShell,
		model.JobTypeCommand: model.TaskTypeCommand,
		model.JobTypeGeneric: model.TaskTypeGeneric,
	}[jobType]
	task := &model.Task{
		TaskID: model.NewTaskID(), JobID: &jobID, TaskType: taskType,
		StartTime: time.Now().UTC().Truncate(time.Millisecond),
	}
	require.NoError(t, db.AddTask(ctx, task))
	alloc := db.RequireMockAllocation(t, pgDB, task.TaskID)

	snapshot := &command.CommandSnapshot{
		TaskID:         task.TaskID,
		RegisteredTime: time.Now().UTC(),
		AllocationID:   alloc.AllocationID,
	}
	if jobType == model.JobTypeGeneric {
		// A generic task stores an empty generic command spec beside its generic task spec.
		snapshot.GenericTaskSpec = &tasks.GenericTaskSpec{WorkspaceID: workspaceID, JobID: jobID}
	} else {
		snapshot.GenericCommandSpec.TaskType = taskType
		snapshot.GenericCommandSpec.Metadata.WorkspaceID = model.AccessScopeID(workspaceID)
	}
	_, err := db.Bun().NewInsert().Model(snapshot).Exec(ctx)
	require.NoError(t, err)
	return jobID
}

// Under RBAC, a job queue update of a colleague's job needs the permission that changes the job on
// its own page, in the job's workspace: UPDATE_EXPERIMENT for an experiment and UPDATE_NSC for a
// shell, command or generic task.
func TestCanControlJobQueueUpdateRBAC(t *testing.T) {
	pgDB, closeDB := db.MustResolveTestPostgres(t)
	defer closeDB()
	useRBACForTest(t)
	ctx := context.Background()
	impl := &JobAuthZRBAC{}

	owner := db.RequireMockUser(t, pgDB)
	editor := db.RequireMockUser(t, pgDB)
	stranger := db.RequireMockUser(t, pgDB)
	clusterAdmin := db.RequireMockUser(t, pgDB)
	wsA, _ := db.RequireMockWorkspaceID(t, pgDB, "")
	wsB, _ := db.RequireMockWorkspaceID(t, pgDB, "")
	projectA, _ := db.RequireMockProjectID(t, pgDB, wsA, false)
	projectB, _ := db.RequireMockProjectID(t, pgDB, wsB, false)
	assignRoleForTest(t, owner, "Editor", ptrs.Ptr(int32(wsA)))
	assignRoleForTest(t, owner, "Editor", ptrs.Ptr(int32(wsB)))
	assignRoleForTest(t, editor, "Editor", ptrs.Ptr(int32(wsA)))
	assignRoleForTest(t, clusterAdmin, "ClusterAdmin", nil)

	jobsIn := func(workspaceID, projectID int) map[string]model.JobID {
		return map[string]model.JobID{
			"experiment": db.RequireMockExperimentProject(t, pgDB, owner, projectID).JobID,
			"shell":      addTaskJobForTest(t, pgDB, owner, model.JobTypeShell, workspaceID),
			"command":    addTaskJobForTest(t, pgDB, owner, model.JobTypeCommand, workspaceID),
			"generic":    addTaskJobForTest(t, pgDB, owner, model.JobTypeGeneric, workspaceID),
		}
	}
	inA, inB := jobsIn(wsA, projectA), jobsIn(wsB, projectB)
	for kind, jobID := range inA {
		require.NoError(t, impl.CanControlJobQueueUpdate(ctx, editor, jobID), kind)
		require.NoError(t, impl.CanControlJobQueueUpdate(ctx, owner, jobID), kind)
		require.NoError(t, impl.CanControlJobQueueUpdate(ctx, clusterAdmin, jobID), kind)
		err := impl.CanControlJobQueueUpdate(ctx, stranger, jobID)
		require.True(t, authz.IsPermissionDenied(err), "%s: %v", kind, err)
	}
	for kind, jobID := range inB {
		err := impl.CanControlJobQueueUpdate(ctx, editor, jobID)
		require.True(t, authz.IsPermissionDenied(err), "%s: %v", kind, err)
		require.NoError(t, impl.CanControlJobQueueUpdate(ctx, owner, jobID), kind)
	}

	// A checkpoint GC job has no workspace and is controlled only with the permission to update
	// the master config.
	gcJob := db.RequireMockJob(t, pgDB, &owner.ID)
	_, err := db.Bun().NewUpdate().Table("jobs").Set("job_type = ?", model.JobTypeCheckpointGC).
		Where("job_id = ?", gcJob).Exec(ctx)
	require.NoError(t, err)
	err = impl.CanControlJobQueueUpdate(ctx, owner, gcJob)
	require.True(t, authz.IsPermissionDenied(err), "%v", err)
	require.NoError(t, impl.CanControlJobQueueUpdate(ctx, clusterAdmin, gcJob))
}
