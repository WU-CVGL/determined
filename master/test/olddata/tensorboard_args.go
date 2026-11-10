//go:build integration
// +build integration

package olddata

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
)

// migrationBeforeTensorBoardArgsMove is the last migration before the one that moves
// tensorboard_args out of the stored configs.
const migrationBeforeTensorBoardArgsMove = 20261110190834

// TensorBoardArgsSpecs are the generic_command_spec values, by task ID, that
// MigrateWithTensorBoardArgs stores in command_state before tensorboard_args is moved.
var TensorBoardArgsSpecs = map[string]string{
	// A shell whose config has the key, which nothing read.
	"olddata-tb-args-shell": `{"TaskType": "SHELL",
		"Base": {"ExtraEnvVars": {"DET_SERVICES": "ssh"}},
		"Config": {"description": "kept", "tensorboard_args": ["--a"], "services": {"ssh": {}}}}`,
	// A shell without the key.
	"olddata-tb-args-untouched": `{"TaskType": "SHELL",
		"Config": {"description": "untouched", "services": {"jupyter": {}}}}`,
}

// TensorBoardArgsTemplates are the template configs, by name, that MigrateWithTensorBoardArgs
// stores before tensorboard_args is moved.
var TensorBoardArgsTemplates = map[string]string{
	// A shell template without a tensorboard entry.
	"olddata-tb-args-template-shell": `{"services": {"jupyter": {}},
		"tensorboard_args": ["--samples_per_plugin=images=5"]}`,
	// A shell template whose tensorboard entry sets no args.
	"olddata-tb-args-template-entry": `{"services": {"tensorboard": {"logdir": "x"}},
		"tensorboard_args": ["--a"]}`,
	// A shell template whose tensorboard entry sets args already.
	"olddata-tb-args-template-set": `{"services": {"tensorboard": {"args": ["a"]}},
		"tensorboard_args": ["b"]}`,
	// A shell template whose tensorboard entry is null.
	"olddata-tb-args-template-null": `{"services": {"ssh": {}, "tensorboard": null},
		"tensorboard_args": ["--a"]}`,
	// A template without services, which may be a command's.
	"olddata-tb-args-template-command": `{"entrypoint": ["sleep", "60"],
		"tensorboard_args": ["--a"]}`,
	// A template with an empty services object, which a command's may have.
	"olddata-tb-args-template-empty-services": `{"services": {}, "tensorboard_args": ["--a"]}`,
	// A value that is not a list, which never decoded.
	"olddata-tb-args-template-string": `{"services": {"jupyter": {}}, "tensorboard_args": "--a"}`,
	// An empty list.
	"olddata-tb-args-template-empty":     `{"services": {"jupyter": {}}, "tensorboard_args": []}`,
	"olddata-tb-args-template-untouched": `{"description": "y"}`,
}

// TensorBoardArgsPolicies are the global invariant configs, by workload type, that
// MigrateWithTensorBoardArgs stores before tensorboard_args is moved.
var TensorBoardArgsPolicies = map[string]string{
	"NTSC":       `{"tensorboard_args": ["--a"], "description": "z"}`,
	"EXPERIMENT": `{"max_restarts": 3}`,
}

// TensorBoardArgsColumns are the stored configs that may hold tensorboard_args, as
// "table.column".
var TensorBoardArgsColumns = []string{
	"command_state.generic_command_spec",
	"templates.config",
	"task_config_policies.invariant_config",
}

// CountTensorBoardArgs returns the number of rows of a column of TensorBoardArgsColumns whose
// JSON names tensorboard_args anywhere.
func CountTensorBoardArgs(t *testing.T, column string) int {
	table, col, ok := strings.Cut(column, ".")
	require.True(t, ok, column)
	var n int
	require.NoError(t, db.Bun().NewRaw(fmt.Sprintf(
		`SELECT count(*) FROM %s WHERE %s::text LIKE '%%tensorboard_args%%'`, table, col),
	).Scan(context.Background(), &n))
	return n
}

// MigrateWithTensorBoardArgs migrates an empty database fully, with the rows of
// TensorBoardArgsSpecs, TensorBoardArgsTemplates and TensorBoardArgsPolicies added before
// tensorboard_args is moved. It returns the counts of CountTensorBoardArgs then, by column.
func MigrateWithTensorBoardArgs(
	t *testing.T, pgdb *db.PgDB, migrationsPath string,
) map[string]int {
	db.MustMigrateTestPostgres(t, pgdb, migrationsPath, "up",
		strconv.Itoa(migrationBeforeTensorBoardArgsMove))
	for taskID, spec := range TensorBoardArgsSpecs {
		pgdb.MustExec(t, `INSERT INTO tasks (task_id, task_type, start_time)
			VALUES ($1, (($2::jsonb)->>'TaskType')::task_type, now())`, taskID, spec)
		pgdb.MustExec(t, `INSERT INTO allocations (task_id, allocation_id, resource_pool, slots)
			VALUES ($1, $1::text || '.1', 'default', 0)`, taskID)
		pgdb.MustExec(t, `INSERT INTO command_state (task_id, allocation_id, generic_command_spec)
			VALUES ($1, $1::text || '.1', $2)`, taskID, spec)
	}
	for name, config := range TensorBoardArgsTemplates {
		pgdb.MustExec(t, `INSERT INTO templates (name, config) VALUES ($1, $2)`, name, config)
	}
	for workloadType, config := range TensorBoardArgsPolicies {
		pgdb.MustExec(t, `INSERT INTO task_config_policies
			(workload_type, last_updated_by, invariant_config) VALUES ($1, 1, $2)`,
			workloadType, config)
	}
	before := map[string]int{}
	for _, column := range TensorBoardArgsColumns {
		before[column] = CountTensorBoardArgs(t, column)
	}
	db.MustMigrateTestPostgres(t, pgdb, migrationsPath, "up")
	return before
}
