//go:build integration
// +build integration

package olddata

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/etc"
)

// The migration deletes the NOTEBOOK tasks with their rows, and keeps a shell's rows.
func TestMigrateDeletesNotebookTasks(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../../static/srv"))
	pgDB, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()

	data := MigrateWithNotebookTasks(t, pgDB, "file://../../static/migrations")

	count := func(query string, args ...any) int {
		var n int
		require.NoError(t, db.Bun().NewRaw(query, args...).Scan(context.Background(), &n), query)
		return n
	}
	for _, tc := range []struct {
		taskID string
		want   int
	}{
		{data.NotebookTaskID, 0},
		{data.ShellTaskID, 1},
	} {
		t.Run(tc.taskID, func(t *testing.T) {
			allocationID := tc.taskID + ".1"
			for _, query := range []string{
				`SELECT count(*) FROM tasks WHERE task_id = ?`,
				`SELECT count(*) FROM allocations WHERE task_id = ?`,
				`SELECT count(*) FROM command_state WHERE task_id = ?`,
				`SELECT count(*) FROM task_logs WHERE task_id = ?`,
				`SELECT count(*) FROM jobs WHERE job_id = ? || '-job'`,
			} {
				require.Equal(t, tc.want, count(query, tc.taskID), query)
			}
			for _, query := range []string{
				`SELECT count(*) FROM task_stats WHERE allocation_id = ?`,
				`SELECT count(*) FROM allocation_sessions WHERE allocation_id = ?`,
			} {
				require.Equal(t, tc.want, count(query, allocationID), query)
			}
		})
	}
	require.Zero(t, count(`SELECT count(*) FROM tasks WHERE task_type = 'NOTEBOOK'`))
	require.Zero(t, count(`SELECT count(*) FROM jobs WHERE job_type = 'NOTEBOOK'`))
}
