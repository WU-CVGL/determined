//go:build integration
// +build integration

package internal

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	pkgCommand "github.com/determined-ai/determined/master/pkg/command"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// launchRows counts the rows that a launch by the user creates: jobs, tasks and allocations of
// the user's jobs, the user's sessions, and the shells that the command service runs.
func launchRows(t *testing.T, userID model.UserID) map[string]int {
	t.Helper()
	ctx := context.Background()
	count := func(query string) int {
		var n int
		require.NoError(t, db.Bun().NewRaw(query, userID).Scan(ctx, &n))
		return n
	}
	shells, err := command.DefaultCmdService.GetShells(&apiv1.GetShellsRequest{})
	require.NoError(t, err)
	return map[string]int{
		"jobs": count(`SELECT count(*) FROM jobs WHERE owner_id = ?`),
		"tasks": count(`SELECT count(*) FROM tasks t JOIN jobs j ON t.job_id = j.job_id
			WHERE j.owner_id = ?`),
		"allocations": count(`SELECT count(*) FROM allocations a
			JOIN tasks t ON a.task_id = t.task_id JOIN jobs j ON t.job_id = j.job_id
			WHERE j.owner_id = ?`),
		"user_sessions": count(`SELECT count(*) FROM user_sessions WHERE user_id = ?`),
		"shells":        len(shells.Shells),
	}
}

// previewConfig returns a config with the given fields.
func previewConfig(t *testing.T, fields map[string]any) *structpb.Struct {
	t.Helper()
	cfg, err := structpb.NewStruct(fields)
	require.NoError(t, err)
	return cfg
}

// setupPreviewTest returns an API server with a command service and code-server, and its user.
func setupPreviewTest(t *testing.T, altMockRM ...*mocks.ResourceManager) (
	*apiServer, model.User, context.Context,
) {
	api, curUser, ctx := setupAPITest(t, nil, altMockRM...)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	withCodeServer(t, api, "4.96.4")
	return api, curUser, ctx
}

// A shell's preview launches nothing: no job, task, allocation, session or shell, however many
// services it declares; the launch of the same request creates them, which shows that the counts
// see them.
func TestPreviewShellLaunchesNothing(t *testing.T) {
	api, curUser, ctx := setupPreviewTest(t)
	cfg := previewConfig(t, map[string]any{
		"services":     map[string]any{"jupyter": map[string]any{}, "vscode": map[string]any{}},
		"idle_timeout": "45m",
	})

	before := launchRows(t, curUser.ID)
	for i := 0; i < 2; i++ {
		_, err := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{Config: cfg})
		require.NoError(t, err)
	}
	require.Equal(t, before, launchRows(t, curUser.ID))

	_, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{Config: cfg})
	require.NoError(t, err)
	after := launchRows(t, curUser.ID)
	for name, n := range before {
		require.Equal(t, n+1, after[name], name)
	}
}

// configWithoutLaunchTail returns a config as a map without what a launch chooses after the
// preparation: the entrypoint, and the description when the request set none.
func configWithoutLaunchTail(cfg *structpb.Struct, describedByRequest bool) map[string]any {
	m := cfg.AsMap()
	delete(m, "entrypoint")
	if !describedByRequest {
		delete(m, "description")
	}
	return m
}

// A shell's preview returns the config that a launch of the same request runs, but for what the
// launch chooses afterwards: the launch's config adds its entrypoint, the supervisor with sshd's
// port, and a description when the request sets none; the services' ports are in the launched
// spec, not in the config. idle_timeout and services.jupyter.idle_type are unset unless the request
// or a template sets them.
func TestPreviewShellIsTheLaunchConfig(t *testing.T) {
	api, _, ctx := setupPreviewTest(t)

	for name, tc := range map[string]struct {
		fields      map[string]any
		idleTimeout bool
		idleType    string
	}{
		"ssh": {fields: map[string]any{}},
		"described, jupyter": {
			fields: map[string]any{
				"description": "mine",
				"services":    map[string]any{"jupyter": map[string]any{}},
			},
		},
		"idle, every service": {
			fields: map[string]any{
				"services": map[string]any{
					"ssh":     map[string]any{},
					"jupyter": map[string]any{"idle_type": model.JupyterIdleTypeKernelConnections},
					"vscode":  map[string]any{},
				},
				"idle_timeout": "45m",
				"resources":    map[string]any{"slots": 0.0},
			},
			idleTimeout: true,
			idleType:    model.JupyterIdleTypeKernelConnections,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := previewConfig(t, tc.fields)
			preview, err := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{Config: cfg})
			require.NoError(t, err)
			previewed := preview.Config.AsMap()
			require.Nil(t, previewed["entrypoint"])
			if tc.idleTimeout {
				require.NotNil(t, previewed["idle_timeout"])
			} else {
				require.Nil(t, previewed["idle_timeout"])
			}
			services, _ := previewed["services"].(map[string]any)
			jupyter, _ := services["jupyter"].(map[string]any)
			idleType, _ := jupyter["idle_type"].(string)
			require.Equal(t, tc.idleType, idleType)
			require.NotContains(t, previewed, "notebook_idle_type")
			require.Empty(t, preview.Warnings)

			launch, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{Config: cfg})
			require.NoError(t, err)
			_, described := tc.fields["description"]
			require.Equal(t, configWithoutLaunchTail(preview.Config, described),
				configWithoutLaunchTail(launch.Config, described))
			if !described {
				require.Empty(t, previewed["description"])
				require.Regexp(t, `^Shell \(.+\)$`, launch.Config.AsMap()["description"])
			}

			// The tail: the supervisor runs sshd at the port of the ssh service, the first one,
			// and each enabled service has its record.
			spec := launchedSpec(t, launch.Shell.Id)
			require.Equal(t, []string{
				interactiveSupervisorScript, "-f", shellSSHDConfigFile,
				"-p", strconv.Itoa(spec.SSHDPort), "-D", "-e",
			}, spec.Config.Entrypoint)
			names := []string{model.ServiceSSH}
			for _, s := range model.EnabledServiceNames(spec.Config.Services) {
				if s != model.ServiceSSH {
					names = append(names, s)
				}
			}
			require.Len(t, spec.Services, len(names))
			for i, s := range spec.Services {
				require.Equal(t, names[i], s.Name)
			}
			require.Equal(t, spec.SSHDPort, spec.Services[0].Port)
		})
	}
}

// A template's services join the request's, in the preview as in the launch.
func TestPreviewShellTemplateServices(t *testing.T) {
	api, _, ctx := setupPreviewTest(t)
	templateName := "shell-services-" + uuid.NewString()
	_, err := db.Bun().NewRaw(
		"INSERT INTO templates (name, config, workspace_id) VALUES (?, ?::jsonb, 1)",
		templateName, `{"services": {"vscode": {}}}`).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Bun().NewDelete().Table("templates").Where("name = ?", templateName).
			Exec(context.Background())
	})
	cfg := servicesConfig(t, map[string]any{"jupyter": map[string]any{}})
	want := map[string]any{"jupyter": map[string]any{}, "vscode": map[string]any{}}

	preview, err := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{
		Config: cfg, TemplateName: templateName,
	})
	require.NoError(t, err)
	require.Equal(t, want, preview.Config.AsMap()["services"])

	launch, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{
		Config: cfg, TemplateName: templateName,
	})
	require.NoError(t, err)
	require.Equal(t, want, launch.Config.AsMap()["services"])
	spec := launchedSpec(t, launch.Shell.Id)
	require.Len(t, spec.Services, 3)
	require.Equal(t, model.ServiceVSCode, spec.Services[2].Name)
}

// A request's service entry keeps the template's idle_type, in the preview as in the launch.
func TestPreviewShellTemplateIdleType(t *testing.T) {
	api, _, ctx := setupPreviewTest(t)
	templateName := "shell-idle-type-" + uuid.NewString()
	_, err := db.Bun().NewRaw(
		"INSERT INTO templates (name, config, workspace_id) VALUES (?, ?::jsonb, 1)",
		templateName, `{"services": {"jupyter": {"idle_type": "kernels_or_terminals"}}}`).
		Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Bun().NewDelete().Table("templates").Where("name = ?", templateName).
			Exec(context.Background())
	})
	cfg := servicesConfig(t, map[string]any{"jupyter": map[string]any{}})
	want := map[string]any{"idle_type": model.JupyterIdleTypeKernelsOrTerminals}

	preview, err := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{
		Config: cfg, TemplateName: templateName,
	})
	require.NoError(t, err)
	require.Equal(t, want, preview.Config.AsMap()["services"].(map[string]any)["jupyter"])

	launch, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{
		Config: cfg, TemplateName: templateName,
	})
	require.NoError(t, err)
	require.Equal(t, want, launch.Config.AsMap()["services"].(map[string]any)["jupyter"])
}

// A request's entry for the tensorboard service keeps the template's logdir and args unless it
// sets them, and its own enabled, in the preview as in the launch: "tensorboard: {}" runs the
// service of a template that turns it off, and an explicit enabled: false is kept.
func TestPreviewShellTemplateTensorBoard(t *testing.T) {
	api, _, ctx := setupPreviewTest(t)
	templateName := "shell-tensorboard-" + uuid.NewString()
	_, err := db.Bun().NewRaw(
		"INSERT INTO templates (name, config, workspace_id) VALUES (?, ?::jsonb, 1)",
		templateName,
		`{"services": {"tensorboard": {"enabled": false, "logdir": "runs", "args": ["--x"]}}}`).
		Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Bun().NewDelete().Table("templates").Where("name = ?", templateName).
			Exec(context.Background())
	})
	for name, tc := range map[string]struct {
		request any
		want    map[string]any
		runs    bool
	}{
		"no request": {
			want: map[string]any{"enabled": false, "logdir": "runs", "args": []any{"--x"}},
		},
		"enabled": {
			request: map[string]any{},
			want:    map[string]any{"logdir": "runs", "args": []any{"--x"}},
			runs:    true,
		},
		"null entry": {
			request: nil,
			want:    map[string]any{"logdir": "runs", "args": []any{"--x"}},
			runs:    true,
		},
		"disabled": {
			request: map[string]any{"enabled": false},
			want:    map[string]any{"enabled": false, "logdir": "runs", "args": []any{"--x"}},
		},
		"own options": {
			request: map[string]any{"logdir": "/logs", "args": []any{}},
			want:    map[string]any{"logdir": "/logs"},
			runs:    true,
		},
	} {
		services := map[string]any{"jupyter": map[string]any{}}
		if name != "no request" {
			services["tensorboard"] = tc.request
		}
		cfg := servicesConfig(t, services)
		preview, err := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{
			Config: cfg, TemplateName: templateName,
		})
		require.NoError(t, err, name)
		require.Equal(t, tc.want,
			preview.Config.AsMap()["services"].(map[string]any)["tensorboard"], name)

		launch, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{
			Config: cfg, TemplateName: templateName,
		})
		require.NoError(t, err, name)
		require.Equal(t, tc.want,
			launch.Config.AsMap()["services"].(map[string]any)["tensorboard"], name)
		spec := launchedSpec(t, launch.Shell.Id)
		_, ok := spec.Service(model.ServiceTensorBoard)
		require.Equal(t, tc.runs, ok, name)
		if tc.runs {
			require.Equal(t, tc.want["logdir"], spec.Base.ExtraEnvVars["DET_TENSORBOARD_LOGDIR"], name)
		}
	}
}

// A shell's preview fills the services that the launch runs by default: jupyter, and vscode only
// when the master serves code-server. A request's or a template's entry is kept, enabled or not:
// a disabled service shows in the config and runs nowhere, and a template's enabled false wins
// over the default, which the request's own entry overrides in turn.
func TestPreviewShellDefaultServices(t *testing.T) {
	api, _, ctx := setupPreviewTest(t)
	off := map[string]any{"enabled": false}
	templateName := "shell-no-jupyter-" + uuid.NewString()
	_, err := db.Bun().NewRaw(
		"INSERT INTO templates (name, config, workspace_id) VALUES (?, ?::jsonb, 1)",
		templateName, `{"services": {"jupyter": {"enabled": false}}}`).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Bun().NewDelete().Table("templates").Where("name = ?", templateName).
			Exec(context.Background())
	})

	for name, tc := range map[string]struct {
		config   *structpb.Struct
		template string
		noCode   bool
		want     map[string]any
		run      []string
	}{
		"no config": {
			want: map[string]any{"jupyter": map[string]any{}, "vscode": map[string]any{}},
			run:  []string{model.ServiceSSH, model.ServiceJupyter, model.ServiceVSCode},
		},
		"empty services": {
			config: servicesConfig(t, map[string]any{}),
			want:   map[string]any{"jupyter": map[string]any{}, "vscode": map[string]any{}},
			run:    []string{model.ServiceSSH, model.ServiceJupyter, model.ServiceVSCode},
		},
		"without code-server": {
			noCode: true,
			want:   map[string]any{"jupyter": map[string]any{}},
			run:    []string{model.ServiceSSH, model.ServiceJupyter},
		},
		"jupyter disabled": {
			config: servicesConfig(t, map[string]any{"jupyter": off}),
			want:   map[string]any{"jupyter": off, "vscode": map[string]any{}},
			run:    []string{model.ServiceSSH, model.ServiceVSCode},
		},
		"both disabled": {
			config: servicesConfig(t, map[string]any{"jupyter": off, "vscode": off}),
			want:   map[string]any{"jupyter": off, "vscode": off},
			run:    []string{model.ServiceSSH},
		},
		"vscode disabled without code-server": {
			config: servicesConfig(t, map[string]any{"vscode": off}),
			noCode: true,
			want:   map[string]any{"jupyter": map[string]any{}, "vscode": off},
			run:    []string{model.ServiceSSH, model.ServiceJupyter},
		},
		"enabled set": {
			config: servicesConfig(t, map[string]any{"jupyter": map[string]any{"enabled": true}}),
			want: map[string]any{
				"jupyter": map[string]any{"enabled": true}, "vscode": map[string]any{},
			},
			run: []string{model.ServiceSSH, model.ServiceJupyter, model.ServiceVSCode},
		},
		"template disables jupyter": {
			template: templateName,
			want:     map[string]any{"jupyter": off, "vscode": map[string]any{}},
			run:      []string{model.ServiceSSH, model.ServiceVSCode},
		},
		"request enables over the template": {
			config:   servicesConfig(t, map[string]any{"jupyter": map[string]any{}}),
			template: templateName,
			want:     map[string]any{"jupyter": map[string]any{}, "vscode": map[string]any{}},
			run:      []string{model.ServiceSSH, model.ServiceJupyter, model.ServiceVSCode},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.noCode {
				prev := api.m.config.Interactive.CodeServer
				api.m.config.Interactive.CodeServer = config.DefaultCodeServerConfig()
				t.Cleanup(func() { api.m.config.Interactive.CodeServer = prev })
			}
			preview, err := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{
				Config: tc.config, TemplateName: tc.template,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, preview.Config.AsMap()["services"])

			launch, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{
				Config: tc.config, TemplateName: tc.template,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, launch.Config.AsMap()["services"])
			spec := launchedSpec(t, launch.Shell.Id)
			var names []string
			for _, s := range spec.Services {
				names = append(names, s.Name)
			}
			require.Equal(t, tc.run, names)
			require.Equal(t, strings.Join(tc.run, ","), spec.Base.ExtraEnvVars["DET_SERVICES"])
		})
	}
}

// A shell's preview refuses what its launch refuses, with the same code and message: an unknown
// service, ssh disabled, vscode while the master serves no code-server, services without a free
// port, an invalid services.jupyter.idle_type or one on another service, the key that it replaces,
// notebook_idle_type, an idle_timeout that is not positive or shorter than the minimum, and a config
// that does not decode.
func TestPreviewShellRefusesAsLaunch(t *testing.T) {
	api, curUser, ctx := setupPreviewTest(t)

	for name, tc := range map[string]struct {
		config *structpb.Struct
		noCode bool
		msg    string
	}{
		"unknown service": {
			config: servicesConfig(t, map[string]any{"emacs": map[string]any{}}),
			msg:    `services: unknown service "emacs"`,
		},
		"ssh disabled": {
			config: servicesConfig(t, map[string]any{"ssh": map[string]any{"enabled": false}}),
			msg:    "services: ssh: can be disabled only for tensorboard with experiment_ids or trial_ids",
		},
		"vscode without code-server": {
			config: servicesConfig(t, map[string]any{"vscode": map[string]any{}}),
			noCode: true,
			msg:    "services: vscode: the master serves no code-server",
		},
		"no port for sshd": {
			config: shellConfig(t, nil, portsExcept(minSshdPort, maxSshdPort)...),
			msg:    "services: ssh: no free port in 3200-3499",
		},
		"no port for vscode": {
			config: shellConfig(t, map[string]any{"jupyter": map[string]any{}, "vscode": map[string]any{}},
				portsExcept(minServicePort, maxServicePort, minServicePort)...),
			msg: "services: vscode: no free port in 2900-3199",
		},
		"jupyter idle_type": {
			config: servicesConfig(t, map[string]any{"jupyter": map[string]any{"idle_type": "sometimes"}}),
			msg:    `services: jupyter: unknown idle_type "sometimes"`,
		},
		"vscode idle_type": {
			config: servicesConfig(t, map[string]any{"vscode": map[string]any{"idle_type": "activity"}}),
			msg:    "services: vscode: only jupyter takes idle_type",
		},
		"notebook_idle_type": {
			config: previewConfig(t, map[string]any{"notebook_idle_type": "activity"}),
			msg:    "notebook_idle_type: use services.jupyter.idle_type",
		},
		"idle_timeout zero": {
			config: previewConfig(t, map[string]any{"idle_timeout": "0s"}),
			msg:    "idle_timeout must be positive",
		},
		"idle_timeout short": {
			config: previewConfig(t, map[string]any{"idle_timeout": "4s"}),
			msg:    "idle_timeout must be at least 10s",
		},
		"unknown field": {
			config: previewConfig(t, map[string]any{"descriptio": "typo"}),
			msg:    `unknown field "descriptio"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.noCode {
				prev := api.m.config.Interactive.CodeServer
				api.m.config.Interactive.CodeServer = config.DefaultCodeServerConfig()
				t.Cleanup(func() { api.m.config.Interactive.CodeServer = prev })
			}
			before := launchRows(t, curUser.ID)
			_, previewErr := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{Config: tc.config})
			require.Equal(t, codes.InvalidArgument, status.Code(previewErr), "%v", previewErr)
			require.ErrorContains(t, previewErr, tc.msg)
			_, launchErr := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{Config: tc.config})
			require.Equal(t, status.Code(launchErr), status.Code(previewErr))
			require.Equal(t, status.Convert(launchErr).Message(), status.Convert(previewErr).Message())
			require.Equal(t, before, launchRows(t, curUser.ID))
		})
	}
}

// A shell's preview returns the warnings that its launch returns.
func TestPreviewShellWarnings(t *testing.T) {
	mockRM := MockRM()
	for _, call := range mockRM.ExpectedCalls {
		if call.Method == validateResourcesMethod {
			call.ReturnArguments = []any{
				[]pkgCommand.LaunchWarning{pkgCommand.CurrentSlotsExceeded}, nil,
			}
		}
	}
	api, _, ctx := setupPreviewTest(t, mockRM)
	want := []apiv1.LaunchWarning{apiv1.LaunchWarning_LAUNCH_WARNING_CURRENT_SLOTS_EXCEEDED}

	cfg := previewConfig(t, map[string]any{"resources": map[string]any{"slots": 64.0}})
	preview, err := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{Config: cfg})
	require.NoError(t, err)
	require.Equal(t, want, preview.Warnings)
	launch, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{Config: cfg})
	require.NoError(t, err)
	require.Equal(t, want, launch.Warnings)
}
