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
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// The migration drops tensorboard_args from every stored config, and moves a non-empty list to
// services.tensorboard.args in a template with a non-empty services object, unless the tensorboard
// entry sets args; a missing or null entry is added disabled. It runs again and changes nothing.
func TestMigrateMovesTensorBoardArgs(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../../static/srv"))
	pgDB, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()

	before := MigrateWithTensorBoardArgs(t, pgDB, "file://../../static/migrations")
	require.Equal(t, map[string]int{
		"command_state.generic_command_spec":    1,
		"templates.config":                      8,
		"task_config_policies.invariant_config": 1,
	}, before)

	wantSpecs := map[string]string{
		"olddata-tb-args-shell": `{"TaskType": "SHELL",
			"Base": {"ExtraEnvVars": {"DET_SERVICES": "ssh"}},
			"Config": {"description": "kept", "services": {"ssh": {}}}}`,
		"olddata-tb-args-untouched": TensorBoardArgsSpecs["olddata-tb-args-untouched"],
	}
	const untouched = "olddata-tb-args-template-untouched"
	wantTemplates := map[string]string{
		"olddata-tb-args-template-shell": `{"services": {"jupyter": {},
			"tensorboard": {"enabled": false, "args": ["--samples_per_plugin=images=5"]}}}`,
		"olddata-tb-args-template-entry": `{"services":
			{"tensorboard": {"logdir": "x", "args": ["--a"]}}}`,
		"olddata-tb-args-template-set": `{"services": {"tensorboard": {"args": ["a"]}}}`,
		"olddata-tb-args-template-null": `{"services": {"ssh": {},
			"tensorboard": {"enabled": false, "args": ["--a"]}}}`,
		"olddata-tb-args-template-command":        `{"entrypoint": ["sleep", "60"]}`,
		"olddata-tb-args-template-empty-services": `{"services": {}}`,
		"olddata-tb-args-template-string":         `{"services": {"jupyter": {}}}`,
		"olddata-tb-args-template-empty":          `{"services": {"jupyter": {}}}`,
		untouched:                                 TensorBoardArgsTemplates[untouched],
	}
	wantPolicies := map[string]string{
		"NTSC":       `{"description": "z"}`,
		"EXPERIMENT": TensorBoardArgsPolicies["EXPERIMENT"],
	}
	require.Len(t, wantSpecs, len(TensorBoardArgsSpecs))
	require.Len(t, wantTemplates, len(TensorBoardArgsTemplates))

	stored := func(query string, arg any) string {
		var s string
		require.NoError(t, db.Bun().NewRaw(query, arg).Scan(context.Background(), &s), query)
		return s
	}
	// rows returns the stored configs as text, by table and key.
	rows := func() map[string]string {
		out := map[string]string{}
		for taskID := range wantSpecs {
			out["command_state "+taskID] = stored(
				`SELECT generic_command_spec::text FROM command_state WHERE task_id = ?`, taskID)
		}
		for name := range wantTemplates {
			out["templates "+name] = stored(`SELECT config::text FROM templates WHERE name = ?`, name)
		}
		for workloadType := range wantPolicies {
			out["task_config_policies "+workloadType] = stored(`SELECT invariant_config::text
				FROM task_config_policies WHERE workspace_id IS NULL AND workload_type = ?`,
				workloadType)
		}
		return out
	}
	migrated := rows()
	for taskID, want := range wantSpecs {
		require.JSONEq(t, want, migrated["command_state "+taskID], taskID)
	}
	for name, want := range wantTemplates {
		require.JSONEq(t, want, migrated["templates "+name], name)
	}
	for workloadType, want := range wantPolicies {
		require.JSONEq(t, want, migrated["task_config_policies "+workloadType], workloadType)
	}
	for _, column := range TensorBoardArgsColumns {
		require.Zero(t, CountTensorBoardArgs(t, column), column)
	}

	// A shell restored across the upgrade decodes its spec, before the migration and after it.
	for _, spec := range []string{
		TensorBoardArgsSpecs["olddata-tb-args-shell"], migrated["command_state olddata-tb-args-shell"],
	} {
		var restored tasks.GenericCommandSpec
		require.NoError(t, json.Unmarshal([]byte(spec), &restored))
		require.Equal(t, "kept", restored.Config.Description)
		config, err := json.Marshal(restored.Config)
		require.NoError(t, err)
		require.NotContains(t, string(config), "tensorboard_args")
	}

	// The migration is idempotent: it runs again and leaves every row as it was.
	migration, err := os.ReadFile(
		"../../static/migrations/20261110190839_move-tensorboard-args.tx.up.sql")
	require.NoError(t, err)
	require.NoError(t, db.Bun().RunInTx(context.Background(), nil,
		func(ctx context.Context, tx bun.Tx) error {
			_, err := tx.ExecContext(ctx, string(migration))
			return err
		}))
	require.Equal(t, migrated, rows())
}
