//go:build integration
// +build integration

package olddata

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/etc"
)

// The migration deletes the TENSORBOARD tasks with their rows, including the job, the agent
// container and the WAITING allocation, and keeps the rows of a shell and of a shell that views
// TensorBoard logs. It runs again and changes nothing.
func TestMigrateDeletesTensorBoardTasks(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../../static/srv"))
	pgDB, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()

	data := MigrateWithTensorBoardTasks(t, pgDB, "file://../../static/migrations")

	count := func(query string, args ...any) int {
		var n int
		require.NoError(t, db.Bun().NewRaw(query, args...).Scan(context.Background(), &n), query)
		return n
	}
	check := func(t *testing.T) {
		for _, tc := range []struct {
			taskID string
			want   int
		}{
			{data.TensorBoardTaskID, 0},
			{data.ShellTaskID, 1},
			{data.SessionTaskID, 1},
		} {
			t.Run(tc.taskID, func(t *testing.T) {
				for _, query := range []string{
					`SELECT count(*) FROM tasks WHERE task_id = ?`,
					`SELECT count(*) FROM allocations WHERE task_id = ?`,
					`SELECT count(*) FROM command_state WHERE task_id = ?`,
					`SELECT count(*) FROM task_logs WHERE task_id = ?`,
					`SELECT count(*) FROM checkpoints_v2 WHERE task_id = ?`,
					`SELECT count(*) FROM jobs WHERE job_id = ? || '-job'`,
					`SELECT count(*) FROM task_stats WHERE allocation_id = ? || '.1'`,
					`SELECT count(*) FROM allocation_sessions WHERE allocation_id = ? || '.1'`,
					`SELECT count(*) FROM allocation_resources WHERE allocation_id = ? || '.1'`,
					`SELECT count(*) FROM resourcemanagers_agent_containers
						WHERE resource_id = ? || '.r'`,
				} {
					require.Equal(t, tc.want, count(query, tc.taskID), query)
				}
			})
		}
		require.Zero(t, count(`SELECT count(*) FROM tasks WHERE task_type = 'TENSORBOARD'`))
		require.Zero(t, count(`SELECT count(*) FROM jobs WHERE job_type = 'TENSORBOARD'`))
		require.Zero(t, count(`SELECT count(*) FROM allocations WHERE state = 'WAITING'`))
		for _, taskID := range []string{data.ShellTaskID, data.SessionTaskID} {
			var spec string
			require.NoError(t, db.Bun().NewRaw(
				`SELECT generic_command_spec::text FROM command_state WHERE task_id = ?`, taskID,
			).Scan(context.Background(), &spec))
			require.JSONEq(t, data.Specs[taskID], spec, taskID)
		}
	}
	check(t)

	migration, err := os.ReadFile(
		"../../static/migrations/20261110010810_delete-tensorboard-tasks.tx.up.sql")
	require.NoError(t, err)
	require.NoError(t, db.Bun().RunInTx(context.Background(), nil,
		func(ctx context.Context, tx bun.Tx) error {
			_, err := tx.ExecContext(ctx, string(migration))
			return err
		}))
	check(t)
}
