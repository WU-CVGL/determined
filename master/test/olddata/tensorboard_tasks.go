//go:build integration
// +build integration

package olddata

import (
	"strconv"
	"testing"

	"github.com/determined-ai/determined/master/internal/db"
)

// migrationBeforeTensorBoardDelete is the last migration before the one that deletes TENSORBOARD
// tasks.
const migrationBeforeTensorBoardDelete = 20261108010744

// TensorBoardTasks names the rows that MigrateWithTensorBoardTasks adds before the TENSORBOARD
// tasks are deleted: a TENSORBOARD job and task whose allocation is WAITING, a SHELL job and task,
// and a SHELL job and task that views TensorBoard logs. Each task has an allocation, command
// state, a task log, task stats, an allocation session, allocation resources with an agent
// container, and a checkpoint. The checkpoints are DELETED ones, since the on_checkpoint_deletion
// trigger keeps any other checkpoint from being deleted.
type TensorBoardTasks struct {
	TensorBoardTaskID string
	ShellTaskID       string
	SessionTaskID     string
	// Specs are the generic_command_spec values of the tasks, by task ID.
	Specs map[string]string
}

// tensorBoardTasksSpecs are the generic_command_spec values of the tasks of TensorBoardTasks.
var tensorBoardTasksSpecs = map[string]string{
	"olddata-tensorboard": `{"TaskType": "TENSORBOARD",
		"Base": {"ExtraEnvVars": {"TENSORBOARD_PORT": "2600"}},
		"Config": {"tensorboard_args": ["--samples_per_plugin=images=5"]}}`,
	"olddata-tb-shell": `{"TaskType": "SHELL", "Config": {"services": {"jupyter": {}}}}`,
	"olddata-tb-session": `{"TaskType": "SHELL", "Config": {"services": {
		"ssh": {"enabled": false}, "tensorboard": {"experiment_ids": [1]}}}}`,
}

const tensorBoardTasksSQL = `
INSERT INTO jobs (job_id, job_type, owner_id) VALUES
    ('olddata-tensorboard-job', 'TENSORBOARD', 1),
    ('olddata-tb-shell-job', 'SHELL', 1),
    ('olddata-tb-session-job', 'SHELL', 1);
INSERT INTO tasks (task_id, job_id, task_type, start_time) VALUES
    ('olddata-tensorboard', 'olddata-tensorboard-job', 'TENSORBOARD', now()),
    ('olddata-tb-shell', 'olddata-tb-shell-job', 'SHELL', now()),
    ('olddata-tb-session', 'olddata-tb-session-job', 'SHELL', now());
INSERT INTO allocations (task_id, allocation_id, resource_pool, slots, state) VALUES
    ('olddata-tensorboard', 'olddata-tensorboard.1', 'default', 0, 'WAITING'),
    ('olddata-tb-shell', 'olddata-tb-shell.1', 'default', 0, 'RUNNING'),
    ('olddata-tb-session', 'olddata-tb-session.1', 'default', 0, 'RUNNING');
INSERT INTO task_logs (task_id, allocation_id, log) VALUES
    ('olddata-tensorboard', 'olddata-tensorboard.1', 'line'),
    ('olddata-tb-shell', 'olddata-tb-shell.1', 'line'),
    ('olddata-tb-session', 'olddata-tb-session.1', 'line');
INSERT INTO task_stats (allocation_id, event_type, start_time, end_time) VALUES
    ('olddata-tensorboard.1', 'QUEUED', now(), now()),
    ('olddata-tb-shell.1', 'QUEUED', now(), now()),
    ('olddata-tb-session.1', 'QUEUED', now(), now());
INSERT INTO allocation_sessions (allocation_id, owner_id) VALUES
    ('olddata-tensorboard.1', 1),
    ('olddata-tb-shell.1', 1),
    ('olddata-tb-session.1', 1);
INSERT INTO allocation_resources (resource_id, allocation_id) VALUES
    ('olddata-tensorboard.r', 'olddata-tensorboard.1'),
    ('olddata-tb-shell.r', 'olddata-tb-shell.1'),
    ('olddata-tb-session.r', 'olddata-tb-session.1');
INSERT INTO resourcemanagers_agent_containers (container_id, resource_id, agent_id) VALUES
    ('olddata-tensorboard.c', 'olddata-tensorboard.r', 'olddata-agent'),
    ('olddata-tb-shell.c', 'olddata-tb-shell.r', 'olddata-agent'),
    ('olddata-tb-session.c', 'olddata-tb-session.r', 'olddata-agent');
INSERT INTO checkpoints_v2 (uuid, task_id, report_time, state) VALUES
    (gen_random_uuid(), 'olddata-tensorboard', now(), 'DELETED'),
    (gen_random_uuid(), 'olddata-tb-shell', now(), 'DELETED'),
    (gen_random_uuid(), 'olddata-tb-session', now(), 'DELETED');
`

// MigrateWithTensorBoardTasks migrates an empty database fully, with the rows of TensorBoardTasks
// added before the TENSORBOARD tasks are deleted.
func MigrateWithTensorBoardTasks(
	t *testing.T, pgdb *db.PgDB, migrationsPath string,
) TensorBoardTasks {
	db.MustMigrateTestPostgres(t, pgdb, migrationsPath, "up",
		strconv.Itoa(migrationBeforeTensorBoardDelete))
	pgdb.MustExec(t, tensorBoardTasksSQL)
	for taskID, spec := range tensorBoardTasksSpecs {
		pgdb.MustExec(t, `INSERT INTO command_state (task_id, allocation_id, generic_command_spec)
			VALUES ($1, $1::text || '.1', $2)`, taskID, spec)
	}
	db.MustMigrateTestPostgres(t, pgdb, migrationsPath, "up")
	return TensorBoardTasks{
		TensorBoardTaskID: "olddata-tensorboard",
		ShellTaskID:       "olddata-tb-shell",
		SessionTaskID:     "olddata-tb-session",
		Specs:             tensorBoardTasksSpecs,
	}
}
