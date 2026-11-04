//go:build integration
// +build integration

package olddata

import (
	"testing"

	"github.com/determined-ai/determined/master/internal/db"
)

// migrationBeforeNotebookDelete is the last migration before the one that deletes NOTEBOOK tasks.
const migrationBeforeNotebookDelete = 20261102000000

// NotebookTasks names the rows that MigrateWithNotebookTasks adds before the NOTEBOOK tasks are
// deleted: a NOTEBOOK job and task, and a SHELL job and task, each with an allocation, command
// state, a task log, task stats, an allocation session and a notebook session.
type NotebookTasks struct {
	NotebookTaskID string
	ShellTaskID    string
}

const notebookTasksSQL = `
INSERT INTO jobs (job_id, job_type, owner_id) VALUES
    ('olddata-notebook-job', 'NOTEBOOK', 1),
    ('olddata-shell-job', 'SHELL', 1);
INSERT INTO tasks (task_id, job_id, task_type, start_time) VALUES
    ('olddata-notebook', 'olddata-notebook-job', 'NOTEBOOK', now()),
    ('olddata-shell', 'olddata-shell-job', 'SHELL', now());
INSERT INTO allocations (task_id, allocation_id, resource_pool, slots) VALUES
    ('olddata-notebook', 'olddata-notebook.1', 'default', 0),
    ('olddata-shell', 'olddata-shell.1', 'default', 0);
INSERT INTO command_state (task_id, allocation_id) VALUES
    ('olddata-notebook', 'olddata-notebook.1'),
    ('olddata-shell', 'olddata-shell.1');
INSERT INTO task_logs (task_id, allocation_id, log) VALUES
    ('olddata-notebook', 'olddata-notebook.1', 'line'),
    ('olddata-shell', 'olddata-shell.1', 'line');
INSERT INTO task_stats (allocation_id, event_type, start_time, end_time) VALUES
    ('olddata-notebook.1', 'QUEUED', now(), now()),
    ('olddata-shell.1', 'QUEUED', now(), now());
INSERT INTO allocation_sessions (allocation_id, owner_id) VALUES
    ('olddata-notebook.1', 1),
    ('olddata-shell.1', 1);
INSERT INTO notebook_sessions (task_id, user_id) VALUES
    ('olddata-notebook', 1),
    ('olddata-shell', 1);
`

// MigrateWithNotebookTasks migrates an empty database fully, with the rows of NotebookTasks added
// before the NOTEBOOK tasks are deleted.
func MigrateWithNotebookTasks(t *testing.T, pgdb *db.PgDB, migrationsPath string) NotebookTasks {
	mustMigrateWithExtras(t, pgdb, migrationsPath, migrationExtra{
		When: migrationBeforeNotebookDelete,
		SQL:  notebookTasksSQL,
	})
	return NotebookTasks{NotebookTaskID: "olddata-notebook", ShellTaskID: "olddata-shell"}
}
