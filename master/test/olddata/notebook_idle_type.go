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

// migrationBeforeNotebookIdleTypeDrop is the last migration before the one that drops
// notebook_idle_type from the stored configs.
const migrationBeforeNotebookIdleTypeDrop = 20261104000000

// NotebookIdleTypeSpecs are the generic_command_spec values, by task ID, that
// MigrateWithNotebookIdleType stores in command_state before notebook_idle_type is dropped.
var NotebookIdleTypeSpecs = map[string]string{
	// A shell with a jupyter entry and a value other than the default.
	"olddata-idle-carried": `{"TaskType": "SHELL",
		"Base": {"ExtraEnvVars": {"NOTEBOOK_PORT": "3142",
			"NOTEBOOK_CONFIG": "{\"notebook_idle_type\":\"kernel_connections\"}"}},
		"Config": {"description": "carried", "notebook_idle_type": "kernel_connections",
			"services": {"ssh": {}, "jupyter": {}}}}`,
	// A shell with the default value.
	"olddata-idle-default": `{"TaskType": "SHELL",
		"Config": {"notebook_idle_type": "activity", "services": {"jupyter": {}}}}`,
	// A shell without a jupyter entry.
	"olddata-idle-no-jupyter": `{"TaskType": "SHELL",
		"Config": {"notebook_idle_type": "kernels_or_terminals", "services": {"ssh": {}}}}`,
	// A shell whose jupyter entry sets an idle type already.
	"olddata-idle-set": `{"TaskType": "SHELL",
		"Config": {"notebook_idle_type": "kernels_or_terminals",
			"services": {"jupyter": {"idle_type": "kernel_connections"}}}}`,
	// A command, which has no services.
	"olddata-idle-command": `{"TaskType": "COMMAND",
		"Config": {"notebook_idle_type": "kernels_or_terminals"}}`,
	// A shell without the key.
	"olddata-idle-untouched": `{"TaskType": "SHELL",
		"Config": {"description": "untouched", "services": {"jupyter": {}}}}`,
}

// NotebookIdleTypeTemplates are the template configs, by name, that MigrateWithNotebookIdleType
// stores before notebook_idle_type is dropped.
var NotebookIdleTypeTemplates = map[string]string{
	"olddata-idle-template-carried": `{"notebook_idle_type": "kernels_or_terminals",
		"services": {"jupyter": null}}`,
	"olddata-idle-template-default":   `{"notebook_idle_type": "activity", "description": "x"}`,
	"olddata-idle-template-untouched": `{"description": "y"}`,
}

// NotebookIdleTypePolicies are the global invariant configs, by workload type, that
// MigrateWithNotebookIdleType stores before notebook_idle_type is dropped.
var NotebookIdleTypePolicies = map[string]string{
	"NTSC":       `{"notebook_idle_type": "kernel_connections", "description": "z"}`,
	"EXPERIMENT": `{"max_restarts": 3}`,
}

// NotebookIdleTypeColumns are the stored configs that may hold notebook_idle_type, as
// "table.column".
var NotebookIdleTypeColumns = []string{
	"command_state.generic_command_spec",
	"templates.config",
	"task_config_policies.invariant_config",
}

// CountNotebookIdleType returns the number of rows of a column of NotebookIdleTypeColumns whose
// JSON names notebook_idle_type anywhere.
func CountNotebookIdleType(t *testing.T, column string) int {
	table, col, ok := strings.Cut(column, ".")
	require.True(t, ok, column)
	var n int
	require.NoError(t, db.Bun().NewRaw(fmt.Sprintf(
		`SELECT count(*) FROM %s WHERE %s::text LIKE '%%notebook_idle_type%%'`, table, col),
	).Scan(context.Background(), &n))
	return n
}

// MigrateWithNotebookIdleType migrates an empty database fully, with the rows of
// NotebookIdleTypeSpecs, NotebookIdleTypeTemplates and NotebookIdleTypePolicies added before
// notebook_idle_type is dropped. It returns the counts of CountNotebookIdleType then, by column.
func MigrateWithNotebookIdleType(
	t *testing.T, pgdb *db.PgDB, migrationsPath string,
) map[string]int {
	db.MustMigrateTestPostgres(t, pgdb, migrationsPath, "up",
		strconv.Itoa(migrationBeforeNotebookIdleTypeDrop))
	for taskID, spec := range NotebookIdleTypeSpecs {
		pgdb.MustExec(t, `INSERT INTO tasks (task_id, task_type, start_time)
			VALUES ($1, (($2::jsonb)->>'TaskType')::task_type, now())`, taskID, spec)
		pgdb.MustExec(t, `INSERT INTO allocations (task_id, allocation_id, resource_pool, slots)
			VALUES ($1, $1::text || '.1', 'default', 0)`, taskID)
		pgdb.MustExec(t, `INSERT INTO command_state (task_id, allocation_id, generic_command_spec)
			VALUES ($1, $1::text || '.1', $2)`, taskID, spec)
	}
	for name, config := range NotebookIdleTypeTemplates {
		pgdb.MustExec(t, `INSERT INTO templates (name, config) VALUES ($1, $2)`, name, config)
	}
	for workloadType, config := range NotebookIdleTypePolicies {
		pgdb.MustExec(t, `INSERT INTO task_config_policies
			(workload_type, last_updated_by, invariant_config) VALUES ($1, 1, $2)`,
			workloadType, config)
	}
	before := map[string]int{}
	for _, column := range NotebookIdleTypeColumns {
		before[column] = CountNotebookIdleType(t, column)
	}
	db.MustMigrateTestPostgres(t, pgdb, migrationsPath, "up")
	return before
}
