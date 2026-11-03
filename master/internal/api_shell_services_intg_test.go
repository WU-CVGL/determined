//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/rm/rmevents"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// servicesConfig returns a launch config with the given services, as the CLI sends
// --config services=...
func servicesConfig(t *testing.T, services any) *structpb.Struct {
	t.Helper()
	config, err := structpb.NewStruct(map[string]any{"services": services})
	require.NoError(t, err)
	return config
}

// shellConfig returns a launch config with the given services, if any, and the given ports in
// environment.proxy_ports.
func shellConfig(t *testing.T, services any, proxyPorts ...int) *structpb.Struct {
	t.Helper()
	ports := make([]any, 0, len(proxyPorts))
	for _, port := range proxyPorts {
		ports = append(ports, map[string]any{"proxy_port": float64(port)})
	}
	raw := map[string]any{"environment": map[string]any{"proxy_ports": ports}}
	if services != nil {
		raw["services"] = services
	}
	config, err := structpb.NewStruct(raw)
	require.NoError(t, err)
	return config
}

// portsExcept returns the ports of [min, max] other than the given ones.
func portsExcept(min, max int, except ...int) []int {
	var ports []int
	for port := min; port <= max; port++ {
		if !slices.Contains(except, port) {
			ports = append(ports, port)
		}
	}
	return ports
}

// requireOnePolicyPerPort checks that every container port and every proxy service ID has one
// proxy policy.
func requireOnePolicyPerPort(t *testing.T, ports []*sproto.ProxyPortConfig) {
	t.Helper()
	byPort, byID := map[int]int{}, map[string]int{}
	for _, p := range ports {
		byPort[p.Port]++
		byID[p.ServiceID]++
	}
	for port, n := range byPort {
		require.Equal(t, 1, n, "port %d has %d proxy policies", port, n)
	}
	for id, n := range byID {
		require.Equal(t, 1, n, "service ID %s has %d proxy policies", id, n)
	}
}

// allocateRequest returns the request with which the master allocated the task's resources.
func allocateRequest(t *testing.T, rm *mocks.ResourceManager, taskID string) sproto.AllocateRequest {
	t.Helper()
	for _, call := range rm.Calls {
		if call.Method != "Allocate" { //nolint:goconst // the mock's method name
			continue
		}
		if req := call.Arguments.Get(0).(sproto.AllocateRequest); string(req.TaskID) == taskID {
			return req
		}
	}
	require.FailNow(t, "no allocation", "task %s was not allocated", taskID)
	return sproto.AllocateRequest{}
}

// containerFilePaths returns the paths of the files that the task's container gets, with how often
// each appears.
func containerFilePaths(spec tasks.GenericCommandSpec) map[string]int {
	paths := map[string]int{}
	for _, a := range spec.ToTaskSpec().ExtraArchives {
		for _, item := range a.Archive {
			paths[item.Path]++
		}
	}
	return paths
}

func notebookSessions(t *testing.T, taskID string) []model.NotebookSession {
	t.Helper()
	var sessions []model.NotebookSession
	require.NoError(t, db.Bun().NewSelect().Model(&sessions).
		Where("task_id = ?", taskID).Scan(context.Background()))
	return sessions
}

func requireOncePerPath(t *testing.T, paths map[string]int) {
	t.Helper()
	for path, n := range paths {
		require.Equal(t, 1, n, "%s appears %d times", path, n)
	}
}

// A shell that declares the jupyter service runs sshd and JupyterLab in one container: ssh is the
// primary service at the bare task ID, jupyter is at "<task>:<port>" behind TLS, a master login and
// the owner rule, with its own port, files, environment and notebook session. Its notebook session
// ends with the shell.
func TestLaunchShellWithJupyterService(t *testing.T) {
	api, curUser, ctx := setupAPITest(t, nil)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	rm := api.m.rm.(*mocks.ResourceManager)
	// The resource manager confirms the release, which ends the allocation.
	rm.On("Release", mock.Anything).Return().Run(func(args mock.Arguments) {
		release := args.Get(0).(sproto.ResourcesReleased)
		if release.ResourcesID == nil {
			rmevents.Publish(release.AllocationID, sproto.ResourcesReleasedEvent{})
		}
	})

	resp, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{
		Config: shellConfig(t, map[string]any{"jupyter": map[string]any{}}, 8080),
	})
	require.NoError(t, err)
	taskID := resp.Shell.Id
	require.NotEmpty(t, resp.Shell.PrivateKey)
	require.Equal(t, map[string]any{"jupyter": map[string]any{}}, resp.Config.AsMap()["services"])
	spec := launchedSpec(t, taskID)

	// The services, the primary one first, with ports from their own ranges.
	require.Len(t, spec.Services, 2)
	ssh, jupyter := spec.Services[0], spec.Services[1]
	require.Equal(t, model.ServiceSSH, ssh.Name)
	require.Equal(t, model.ServiceJupyter, jupyter.Name)
	require.GreaterOrEqual(t, ssh.Port, minSshdPort)
	require.LessOrEqual(t, ssh.Port, maxSshdPort)
	require.GreaterOrEqual(t, jupyter.Port, minNotebookPort)
	require.LessOrEqual(t, jupyter.Port, maxNotebookPort)
	require.NotEqual(t, ssh.Port, jupyter.Port)
	require.Equal(t, ssh.Port, spec.SSHDPort)
	require.Equal(t, []string{
		interactiveSupervisorScript, "-f", shellSSHDConfigFile, "-p", strconv.Itoa(ssh.Port),
		"-D", "-e",
	}, spec.Config.Entrypoint)
	// The services' ports are in their records only.
	require.Empty(t, spec.Base.ExtraProxyPorts)

	// The proxies, one per port: sshd as the shell's default TCP port without a master login,
	// JupyterLab over HTTPS for its owner and admins, under its own prefix, and the config's port
	// plain.
	jupyterID := taskID + ":" + strconv.Itoa(jupyter.Port)
	alloc := allocateRequest(t, rm, taskID)
	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: taskID, Port: ssh.Port, ProxyTCP: true, Unauthenticated: true},
		{ServiceID: jupyterID, Port: jupyter.Port, TLS: true, Exec: true},
		{ServiceID: taskID + ":8080", Port: 8080},
	}, alloc.ProxyPorts)
	// Without an idle timeout, the shell's idle time is not watched.
	require.Nil(t, alloc.IdleTimeout)
	require.True(t, spec.WatchTaskActivity)
	require.False(t, spec.WatchProxyIdleTimeout)
	require.False(t, spec.WatchRunnerIdleTimeout)

	// The environment of both services, merged with the others.
	env := spec.Base.ExtraEnvVars
	require.Equal(t, "ssh,jupyter", env["DET_SERVICES"])
	require.Equal(t, string(model.TaskTypeShell), env["DET_TASK_TYPE"])
	require.Equal(t, strconv.Itoa(jupyter.Port), env["NOTEBOOK_PORT"])
	require.Equal(t, "/proxy/"+jupyterID+"/", env["NOTEBOOK_BASE_URL"])
	var notebookConfig model.CommandConfig
	require.NoError(t, json.Unmarshal([]byte(env["NOTEBOOK_CONFIG"]), &notebookConfig))
	require.Equal(t, spec.Config.Description, notebookConfig.Description)
	// Nor does JupyterLab's idle check run.
	require.NotContains(t, env, tasks.NotebookIdleTypeEnvVar)
	require.NotContains(t, env, tasks.ActivityReportEnvVar)
	require.NotContains(t, env, tasks.IdleTimeoutSecondsEnvVar)

	// JupyterLab's token is a notebook session of the owner.
	require.NotEmpty(t, env[model.NotebookSessionEnvVar])
	sessions := notebookSessions(t, taskID)
	require.Len(t, sessions, 1)
	require.Equal(t, curUser.ID, sessions[0].UserID)

	// The files of both services, each once.
	paths := containerFilePaths(spec)
	requireOncePerPath(t, paths)
	for _, path := range []string{
		shellEntrypointScript, taskReadyCheckLogs, interactiveSupervisorScript,
		"/run/determined/services/ssh.sh", "/run/determined/services/jupyter.sh",
		"/run/determined/ssh/authorized_keys_unmodified", "/run/determined/ssh/sshd_config",
		jupyterDir, jupyterConfigDir, jupyterDataDir, jupyterRuntimeDir, jupyterIdleCheck,
		notebookDefaultPage, jupyterKeyPath, jupyterCertPath,
	} {
		require.Contains(t, paths, path)
	}
	require.NotContains(t, paths, jupyterEntrypoint)

	// The shell's notebook session ends with it.
	_, err = api.KillShell(ctx, &apiv1.KillShellRequest{ShellId: taskID})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(notebookSessions(t, taskID)) == 0 },
		10*time.Second, 50*time.Millisecond)
}

// Without extra services a shell is launched as before: sshd only, at the bare task ID, without a
// notebook session or JupyterLab's environment and files. Listing ssh or an empty map changes
// nothing.
func TestLaunchShellWithoutExtraServices(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	rm := api.m.rm.(*mocks.ResourceManager)

	for name, config := range map[string]*structpb.Struct{
		"unset":     nil,
		"empty":     servicesConfig(t, map[string]any{}),
		"ssh":       servicesConfig(t, map[string]any{"ssh": map[string]any{}}),
		"ssh, null": servicesConfig(t, map[string]any{"ssh": nil}),
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{Config: config})
			require.NoError(t, err)
			taskID := resp.Shell.Id
			spec := launchedSpec(t, taskID)

			require.Len(t, spec.Services, 1)
			port := spec.Services[0].Port
			require.Equal(t, sshService(port), spec.Services[0])
			require.Equal(t, port, spec.SSHDPort)
			require.Empty(t, spec.Base.ExtraProxyPorts)
			alloc := allocateRequest(t, rm, taskID)
			require.Equal(t, []*sproto.ProxyPortConfig{
				{ServiceID: taskID, Port: port, ProxyTCP: true, Unauthenticated: true},
			}, alloc.ProxyPorts)
			// No idle timeout, no reaping.
			require.Nil(t, alloc.IdleTimeout)

			env := spec.Base.ExtraEnvVars
			require.Equal(t, "ssh", env["DET_SERVICES"])
			for _, key := range []string{
				model.NotebookSessionEnvVar, "NOTEBOOK_PORT", "NOTEBOOK_CONFIG", "NOTEBOOK_BASE_URL",
				tasks.NotebookIdleTypeEnvVar, tasks.ActivityReportEnvVar,
				tasks.IdleTimeoutSecondsEnvVar,
			} {
				require.NotContains(t, env, key)
			}
			require.Empty(t, notebookSessions(t, taskID))

			paths := containerFilePaths(spec)
			requireOncePerPath(t, paths)
			require.NotContains(t, paths, jupyterDir)
			require.NotContains(t, paths, jupyterCertPath)
		})
	}
}

// A shell with an idle timeout is watched through its task's activity record, with or without
// extra services; with jupyter, JupyterLab's idle check reports there with the timeout and the
// idle type, activity unless the template or the config sets another. A notebook's default idle
// type is TestLaunchNotebookProxiesUnchanged's.
func TestLaunchShellIdleTimeout(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	rm := api.m.rm.(*mocks.ResourceManager)
	jupyter := map[string]any{"jupyter": map[string]any{}}

	for name, tc := range map[string]struct {
		services     map[string]any
		idleType     string
		template     string
		wantIdleType string
	}{
		"ssh":     {},
		"jupyter": {services: jupyter, wantIdleType: model.NotebookIdleTypeActivity},
		"jupyter configured": {
			services:     jupyter,
			idleType:     model.NotebookIdleTypeKernelConnections,
			wantIdleType: model.NotebookIdleTypeKernelConnections,
		},
		"jupyter template": {
			services:     jupyter,
			template:     `{"notebook_idle_type": "kernels_or_terminals"}`,
			wantIdleType: model.NotebookIdleTypeKernelsOrTerminals,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var templateName string
			if tc.template != "" {
				templateName = "shell-idle-" + uuid.NewString()
				_, err := db.Bun().NewRaw(
					"INSERT INTO templates (name, config, workspace_id) VALUES (?, ?::jsonb, 1)",
					templateName, tc.template).Exec(context.Background())
				require.NoError(t, err)
				t.Cleanup(func() {
					_, _ = db.Bun().NewDelete().Table("templates").Where("name = ?", templateName).
						Exec(context.Background())
				})
			}
			config := map[string]any{"idle_timeout": "45m"}
			if tc.services != nil {
				config["services"] = tc.services
			}
			if tc.idleType != "" {
				config["notebook_idle_type"] = tc.idleType
			}
			raw, err := structpb.NewStruct(config)
			require.NoError(t, err)
			resp, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{
				Config: raw, TemplateName: templateName,
			})
			require.NoError(t, err)
			taskID := resp.Shell.Id
			spec := launchedSpec(t, taskID)

			require.Equal(t, &sproto.IdleTimeoutConfig{
				ServiceID: taskID, TaskID: model.TaskID(taskID), UseTaskActivity: true,
				TimeoutDuration: 45 * time.Minute,
			}, allocateRequest(t, rm, taskID).IdleTimeout)
			require.False(t, spec.WatchProxyIdleTimeout)
			require.False(t, spec.WatchRunnerIdleTimeout)

			env := spec.Base.ExtraEnvVars
			if tc.services == nil {
				require.NotContains(t, env, tasks.NotebookIdleTypeEnvVar)
				require.NotContains(t, env, tasks.ActivityReportEnvVar)
				require.NotContains(t, env, tasks.IdleTimeoutSecondsEnvVar)
				return
			}
			require.Equal(t, tc.wantIdleType, env[tasks.NotebookIdleTypeEnvVar])
			require.Equal(t, tc.wantIdleType, spec.Config.NotebookIdleType)
			require.Equal(t, tasks.ActivityReportTask, env[tasks.ActivityReportEnvVar])
			require.Equal(t, "2700", env[tasks.IdleTimeoutSecondsEnvVar])
		})
	}
}

// A shell refuses an idle_timeout shorter than the idle watcher's tick, with or without jupyter,
// and takes one of exactly that length.
func TestLaunchShellIdleTimeoutMinimum(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	rm := api.m.rm.(*mocks.ResourceManager)

	for _, services := range []map[string]any{nil, {"jupyter": map[string]any{}}} {
		launch := func(timeout string) (*apiv1.LaunchShellResponse, error) {
			config := map[string]any{"idle_timeout": timeout}
			if services != nil {
				config["services"] = services
			}
			raw, err := structpb.NewStruct(config)
			require.NoError(t, err)
			return api.LaunchShell(ctx, &apiv1.LaunchShellRequest{Config: raw})
		}
		for _, timeout := range []string{"1ns", "1s", "4.9s"} {
			_, err := launch(timeout)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", timeout, err)
			require.ErrorContains(t, err, "idle_timeout must be at least 5s", timeout)
		}
		resp, err := launch("5s")
		require.NoError(t, err)
		require.Equal(t, 5*time.Second,
			allocateRequest(t, rm, resp.Shell.Id).IdleTimeout.TimeoutDuration)
	}
}

// A service's port is never one of the config's proxy ports: with every port of both ranges but one
// declared, sshd and JupyterLab get the ones left, and each port has one proxy policy.
func TestLaunchShellServicePortsAvoidProxyPorts(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	rm := api.m.rm.(*mocks.ResourceManager)

	sshPort, jupyterPort := minSshdPort+42, minNotebookPort+137
	declared := append(portsExcept(minSshdPort, maxSshdPort, sshPort),
		portsExcept(minNotebookPort, maxNotebookPort, jupyterPort)...)
	resp, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{
		Config: shellConfig(t, map[string]any{"jupyter": map[string]any{}}, declared...),
	})
	require.NoError(t, err)
	taskID := resp.Shell.Id
	spec := launchedSpec(t, taskID)

	require.Len(t, spec.Services, 2)
	require.Equal(t, sshPort, spec.Services[0].Port)
	require.Equal(t, sshPort, spec.SSHDPort)
	require.Equal(t, jupyterPort, spec.Services[1].Port)
	require.Equal(t, strconv.Itoa(jupyterPort), spec.Base.ExtraEnvVars["NOTEBOOK_PORT"])

	ports := allocateRequest(t, rm, taskID).ProxyPorts
	require.Len(t, ports, 2+len(declared))
	requireOnePolicyPerPort(t, ports)
	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: taskID, Port: sshPort, ProxyTCP: true, Unauthenticated: true},
		{ServiceID: taskID + ":" + strconv.Itoa(jupyterPort), Port: jupyterPort, TLS: true, Exec: true},
	}, ports[:2])
	for _, p := range ports[2:] {
		require.Equal(t, &sproto.ProxyPortConfig{
			ServiceID: taskID + ":" + strconv.Itoa(p.Port), Port: p.Port,
		}, p)
	}
}

// When the config's proxy ports take every port of a service's range, the launch fails and names
// the range.
func TestLaunchShellServicePortsExhausted(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	jupyter := map[string]any{"jupyter": map[string]any{}}

	for name, tc := range map[string]struct {
		config *structpb.Struct
		msg    string
	}{
		"ssh": {
			shellConfig(t, nil, portsExcept(minSshdPort, maxSshdPort)...),
			"services: ssh: no free port in 3200-3499",
		},
		"jupyter": {
			shellConfig(t, jupyter, portsExcept(minNotebookPort, maxNotebookPort)...),
			"services: jupyter: no free port in 2900-3199",
		},
	} {
		_, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{Config: tc.config})
		require.Error(t, err, name)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", name, err)
		require.ErrorContains(t, err, tc.msg, name)
	}

	// Without jupyter, the notebooks' range is free to declare.
	_, err = api.LaunchShell(ctx, &apiv1.LaunchShellRequest{
		Config: shellConfig(t, nil, portsExcept(minNotebookPort, maxNotebookPort)...),
	})
	require.NoError(t, err)
}

// A notebook keeps its launch: JupyterLab at the bare task ID, every port over HTTPS, no service
// records, and the idle check's environment; its base URL is the one JupyterLab had.
func TestLaunchNotebookProxiesUnchanged(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	rm := api.m.rm.(*mocks.ResourceManager)

	resp, err := api.LaunchNotebook(ctx, &apiv1.LaunchNotebookRequest{})
	require.NoError(t, err)
	taskID := resp.Notebook.Id
	spec := launchedSpec(t, taskID)
	require.Empty(t, spec.Services)
	require.Len(t, spec.Base.ExtraProxyPorts, 1)
	port := spec.Base.ExtraProxyPorts[0].ProxyPort()
	alloc := allocateRequest(t, rm, taskID)
	require.Equal(t, []*sproto.ProxyPortConfig{{ServiceID: taskID, Port: port, TLS: true}},
		alloc.ProxyPorts)
	require.True(t, spec.WatchProxyIdleTimeout)
	require.True(t, spec.WatchRunnerIdleTimeout)
	require.False(t, spec.WatchTaskActivity)
	// The master sets no notebook_timeout here, so the notebook has no idle timeout.
	require.Nil(t, alloc.IdleTimeout)

	env := spec.Base.ExtraEnvVars
	require.Equal(t, "jupyter", env["DET_SERVICES"])
	require.Equal(t, "/proxy/"+taskID+"/", env["NOTEBOOK_BASE_URL"])
	require.Equal(t, model.NotebookIdleTypeKernelsOrTerminals, env["NOTEBOOK_IDLE_TYPE"])
	require.NotContains(t, env, tasks.ActivityReportEnvVar)
	require.NotContains(t, env, tasks.IdleTimeoutSecondsEnvVar)
	require.Len(t, notebookSessions(t, taskID), 1)
	paths := containerFilePaths(spec)
	requireOncePerPath(t, paths)
	require.Contains(t, paths, jupyterEntrypoint)
	require.NotContains(t, paths, "/run/determined/ssh/authorized_keys_unmodified")

	// With an idle timeout, the notebook's watcher reads its proxy and its idle reports, as before.
	raw, err := structpb.NewStruct(map[string]any{"idle_timeout": "45m"})
	require.NoError(t, err)
	resp, err = api.LaunchNotebook(ctx, &apiv1.LaunchNotebookRequest{Config: raw})
	require.NoError(t, err)
	require.Equal(t, &sproto.IdleTimeoutConfig{
		ServiceID: resp.Notebook.Id, UseProxyState: true, UseRunnerState: true,
		TimeoutDuration: 45 * time.Minute,
	}, allocateRequest(t, rm, resp.Notebook.Id).IdleTimeout)
}

// Only shells take services, and only the ones the supervisor has units for, with no options.
func TestLaunchRejectsServices(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	jupyter := map[string]any{"jupyter": map[string]any{}}

	// The message ties each rejection to the services check: a launch can be refused with
	// InvalidArgument for other reasons, such as TensorBoard's experiment 1 not existing.
	requireInvalid := func(err error, what, msg string) {
		t.Helper()
		require.Error(t, err, what)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", what, err)
		require.ErrorContains(t, err, msg, what)
	}
	const onlyShells = "services: only shells run services"
	_, err = api.LaunchNotebook(ctx, &apiv1.LaunchNotebookRequest{Config: servicesConfig(t, jupyter)})
	requireInvalid(err, "notebook", onlyShells)
	_, err = api.LaunchNotebook(ctx, &apiv1.LaunchNotebookRequest{
		Config: servicesConfig(t, jupyter), Preview: true,
	})
	requireInvalid(err, "notebook preview", onlyShells)
	_, err = api.LaunchCommand(ctx, &apiv1.LaunchCommandRequest{Config: servicesConfig(t, jupyter)})
	requireInvalid(err, "command", onlyShells)
	_, err = api.LaunchTensorboard(ctx, &apiv1.LaunchTensorboardRequest{
		ExperimentIds: []int32{1}, Config: servicesConfig(t, jupyter),
	})
	requireInvalid(err, "tensorboard", onlyShells)

	for name, tc := range map[string]struct {
		services any
		msg      string
	}{
		"unknown": {
			map[string]any{"vscode": map[string]any{}}, `services: unknown service "vscode"`,
		},
		"not an object": {map[string]any{"jupyter": 1.0}, "model.ServiceConfig"},
		"an option": {
			map[string]any{"jupyter": map[string]any{"port": 8888.0}}, `unknown field "port"`,
		},
		"a list": {[]any{"jupyter"}, "services"},
	} {
		_, err = api.LaunchShell(ctx, &apiv1.LaunchShellRequest{
			Config: servicesConfig(t, tc.services),
		})
		requireInvalid(err, "shell: "+name, tc.msg)
	}

	// An empty map is no services, on every launch.
	_, err = api.LaunchCommand(ctx, &apiv1.LaunchCommandRequest{
		Config: servicesConfig(t, map[string]any{}),
	})
	require.NoError(t, err)
}
