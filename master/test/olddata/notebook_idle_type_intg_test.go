//go:build integration
// +build integration

package olddata

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// The migration drops notebook_idle_type from every stored config, and moves a value other than
// the default to services.jupyter.idle_type when the jupyter entry has none: in a stored task's
// config, when it has a jupyter entry; in a template's, when it has a non-empty services object,
// with a jupyter entry or without one.
func TestMigrateDropsNotebookIdleType(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../../static/srv"))
	pgDB, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()

	before := MigrateWithNotebookIdleType(t, pgDB, "file://../../static/migrations")
	require.Equal(t, map[string]int{
		"command_state.generic_command_spec":    5,
		"templates.config":                      7,
		"task_config_policies.invariant_config": 1,
	}, before)

	wantSpecs := map[string]string{
		"olddata-idle-carried": `{"TaskType": "SHELL",
			"Base": {"ExtraEnvVars": {"NOTEBOOK_PORT": "3142"}},
			"Config": {"description": "carried",
				"services": {"ssh": {}, "jupyter": {"idle_type": "kernel_connections"}}}}`,
		"olddata-idle-default": `{"TaskType": "SHELL",
			"Config": {"services": {"jupyter": {}}}}`,
		"olddata-idle-no-jupyter": `{"TaskType": "SHELL",
			"Config": {"services": {"ssh": {}}}}`,
		"olddata-idle-set": `{"TaskType": "SHELL",
			"Config": {"services": {"jupyter": {"idle_type": "kernel_connections"}}}}`,
		"olddata-idle-command":   `{"TaskType": "COMMAND", "Config": {}}`,
		"olddata-idle-untouched": NotebookIdleTypeSpecs["olddata-idle-untouched"],
	}
	wantTemplates := map[string]string{
		"olddata-idle-template-carried": `{"services":
			{"jupyter": {"idle_type": "kernels_or_terminals"}}}`,
		"olddata-idle-template-vscode-off": `{"idle_timeout": "30m", "services": {
			"vscode": {"enabled": false}, "jupyter": {"idle_type": "kernels_or_terminals"}}}`,
		"olddata-idle-template-ssh": `{"services":
			{"ssh": {}, "jupyter": {"idle_type": "kernel_connections"}}}`,
		"olddata-idle-template-set": `{"services":
			{"jupyter": {"idle_type": "kernel_connections"}}}`,
		"olddata-idle-template-command":        `{"entrypoint": ["sleep", "60"]}`,
		"olddata-idle-template-empty-services": `{"services": {}}`,
		"olddata-idle-template-default":        `{"description": "x"}`,
		"olddata-idle-template-untouched":      NotebookIdleTypeTemplates["olddata-idle-template-untouched"],
	}
	wantPolicies := map[string]string{
		"NTSC":       `{"description": "z"}`,
		"EXPERIMENT": NotebookIdleTypePolicies["EXPERIMENT"],
	}
	require.Len(t, wantSpecs, len(NotebookIdleTypeSpecs))
	require.Len(t, wantTemplates, len(NotebookIdleTypeTemplates))

	stored := func(query string, arg any) string {
		var s string
		require.NoError(t, db.Bun().NewRaw(query, arg).Scan(context.Background(), &s), query)
		return s
	}
	check := func(t *testing.T) {
		for taskID, want := range wantSpecs {
			require.JSONEq(t, want, stored(
				`SELECT generic_command_spec::text FROM command_state WHERE task_id = ?`, taskID),
				taskID)
		}
		for name, want := range wantTemplates {
			require.JSONEq(t, want,
				stored(`SELECT config::text FROM templates WHERE name = ?`, name), name)
		}
		for workloadType, want := range wantPolicies {
			require.JSONEq(t, want, stored(`SELECT invariant_config::text FROM task_config_policies
				WHERE workspace_id IS NULL AND workload_type = ?`, workloadType), workloadType)
		}
		for _, column := range NotebookIdleTypeColumns {
			require.Zero(t, CountNotebookIdleType(t, column), column)
		}
	}
	check(t)

	// A shell restored across the upgrade decodes its spec, before the migration and after it.
	// Its config, which GET /api/v1/shells/{id} returns, never has the old key; the migration keeps
	// the idle type under services.jupyter, where the decode before it falls back to the default.
	for spec, want := range map[string]string{
		NotebookIdleTypeSpecs["olddata-idle-carried"]: model.JupyterIdleTypeActivity,
		stored(`SELECT generic_command_spec::text FROM command_state WHERE task_id = ?`,
			"olddata-idle-carried"): model.JupyterIdleTypeKernelConnections,
	} {
		var restored tasks.GenericCommandSpec
		require.NoError(t, json.Unmarshal([]byte(spec), &restored))
		require.Equal(t, want, restored.Config.Services[model.ServiceJupyter].JupyterIdleType())
		config, err := json.Marshal(restored.Config)
		require.NoError(t, err)
		require.NotContains(t, string(config), "notebook_idle_type")
	}

	// The migration is idempotent: it runs again and changes nothing.
	migration, err := os.ReadFile(
		"../../static/migrations/20261104000100_drop-notebook-idle-type.tx.up.sql")
	require.NoError(t, err)
	require.NoError(t, db.Bun().RunInTx(context.Background(), nil,
		func(ctx context.Context, tx bun.Tx) error {
			_, err := tx.ExecContext(ctx, string(migration))
			return err
		}))
	check(t)
}
