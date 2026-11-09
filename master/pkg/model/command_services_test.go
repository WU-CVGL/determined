package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

// decodeCommandConfig decodes text over the default config strictly, as the command API decodes a
// request's config.
func decodeCommandConfig(text string) (CommandConfig, error) {
	config := DefaultConfig(nil)
	config.Entrypoint = []string{"run"}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	err := dec.Decode(&config)
	return config, err
}

func TestCommandConfigServices(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		want         []string
	}{
		{name: "unset", config: `{}`},
		{name: "empty map", config: `{"services": {}}`},
		{name: "null", config: `{"services": null}`},
		{name: "jupyter", config: `{"services": {"jupyter": {}}}`, want: []string{"jupyter"}},
		{name: "null options", config: `{"services": {"jupyter": null}}`, want: []string{"jupyter"}},
		{
			name:   "ssh listed",
			config: `{"services": {"ssh": {}, "jupyter": {}}}`,
			want:   []string{"jupyter", "ssh"},
		},
		{
			name:   "vscode",
			config: `{"services": {"vscode": {}, "jupyter": {}}}`,
			want:   []string{"jupyter", "vscode"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := decodeCommandConfig(tc.config)
			require.NoError(t, err)
			require.NoError(t, check.Validate(config))
			if tc.want == nil {
				require.Empty(t, config.Services)
			} else {
				require.Equal(t, tc.want, EnabledServiceNames(config.Services))
			}
		})
	}

	// A service that the supervisor has no unit for.
	for _, name := range []string{"emacs", "VSCode", "SSH", ""} {
		config, err := decodeCommandConfig(`{"services": {"` + name + `": {}}}`)
		require.NoError(t, err, name)
		err = check.Validate(config)
		require.ErrorContains(t, err, `services: unknown service "`+name+`"`)
	}

	// A service's value is an object of options: "enabled", "idle_type", and no other.
	for _, value := range []string{
		`1`, `"on"`, `true`, `[]`, `{"port": 8888}`, `{"enabled": 1}`, `{"enabled": "no"}`,
		`{"idle_type": 1}`,
	} {
		_, err := decodeCommandConfig(`{"services": {"jupyter": ` + value + `}}`)
		require.Error(t, err, value)
	}
	_, err := decodeCommandConfig(`{"services": ["jupyter"]}`)
	require.Error(t, err)
}

// A service runs unless its config sets enabled to false; its entry stays in the config either
// way, and a disabled one is not among the enabled names.
func TestServiceConfigEnabled(t *testing.T) {
	require.True(t, ServiceConfig{}.Runs())
	require.True(t, ServiceConfig{Enabled: ptrs.Ptr(true)}.Runs())
	require.False(t, ServiceConfig{Enabled: ptrs.Ptr(false)}.Runs())

	config, err := decodeCommandConfig(
		`{"services": {"jupyter": {"enabled": false}, "vscode": {"enabled": true}, "ssh": {}}}`)
	require.NoError(t, err)
	require.NoError(t, check.Validate(config))
	require.Equal(t, []string{"ssh", "vscode"}, EnabledServiceNames(config.Services))
	require.Equal(t, []string{"jupyter", "ssh", "vscode"}, configuredServiceNames(config.Services))
	require.Len(t, config.Services, 3)
	require.False(t, config.Services["jupyter"].Runs())

	// The entry round-trips: enabled is written only when set.
	raw, err := json.Marshal(config.Services)
	require.NoError(t, err)
	require.JSONEq(t, `{"jupyter": {"enabled": false}, "vscode": {"enabled": true}, "ssh": {}}`,
		string(raw))

	require.Empty(t, EnabledServiceNames(nil))
	require.Empty(t, EnabledServiceNames(map[string]ServiceConfig{
		"jupyter": {Enabled: ptrs.Ptr(false)},
	}))
}

// A shell runs its SSH server unless it views TensorBoard logs. An unknown service is refused
// whether it is enabled or not.
func TestCommandConfigServicesRefusals(t *testing.T) {
	config, err := decodeCommandConfig(`{"services": {"ssh": {"enabled": false}}}`)
	require.NoError(t, err)
	err = check.Validate(config)
	require.ErrorContains(t, err,
		"services: ssh: can be disabled only for tensorboard with experiment_ids or trial_ids")
	config, err = decodeCommandConfig(`{"services": {"ssh": {"enabled": true}}}`)
	require.NoError(t, err)
	require.NoError(t, check.Validate(config))

	for _, value := range []string{`{}`, `{"enabled": false}`, `{"enabled": true}`} {
		config, err := decodeCommandConfig(`{"services": {"emacs": ` + value + `}}`)
		require.NoError(t, err, value)
		err = check.Validate(config)
		require.ErrorContains(t, err, `services: unknown service "emacs"`, value)
	}
}

// The jupyter service's idle_type is one of the idle types, activity when unset; no other service
// takes it. The config has no top-level notebook_idle_type.
func TestServiceConfigIdleType(t *testing.T) {
	require.Equal(t, JupyterIdleTypeActivity, ServiceConfig{}.JupyterIdleType())
	for _, idleType := range JupyterIdleTypes {
		config, err := decodeCommandConfig(`{"services": {"jupyter": {"idle_type": "` + idleType + `"}}}`)
		require.NoError(t, err, idleType)
		require.NoError(t, check.Validate(config), idleType)
		require.Equal(t, idleType, config.Services[ServiceJupyter].JupyterIdleType())
	}
	config, err := decodeCommandConfig(`{"services": {"jupyter": {"enabled": false}}}`)
	require.NoError(t, err)
	require.Equal(t, JupyterIdleTypeActivity, config.Services[ServiceJupyter].JupyterIdleType())
	raw, err := json.Marshal(config.Services)
	require.NoError(t, err)
	require.JSONEq(t, `{"jupyter": {"enabled": false}}`, string(raw), "an unset idle_type is not stored")

	for text, want := range map[string]string{
		`{"services": {"jupyter": {"idle_type": "sometimes"}}}`: `services: jupyter: unknown idle_type "sometimes"`,
		`{"services": {"jupyter": {"idle_type": "Activity"}}}`:  `services: jupyter: unknown idle_type "Activity"`,
		`{"services": {"vscode": {"idle_type": "activity"}}}`:   "services: vscode: only jupyter takes idle_type",
		`{"services": {"ssh": {"idle_type": "activity"}}}`:      "services: ssh: only jupyter takes idle_type",
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.ErrorContains(t, check.Validate(config), want, text)
	}

	_, err = decodeCommandConfig(`{"notebook_idle_type": "activity"}`)
	require.ErrorContains(t, err, `unknown field "notebook_idle_type"`)
}

// The default services of a shell: jupyter, and vscode when the master serves code-server.
func TestDefaultShellServices(t *testing.T) {
	require.Equal(t, []string{ServiceJupyter}, DefaultShellServices(false))
	require.Equal(t, []string{ServiceJupyter, ServiceVSCode}, DefaultShellServices(true))
}

// An idle_timeout can be unset, and is positive when set.
func TestCommandConfigIdleTimeout(t *testing.T) {
	for _, text := range []string{
		`{}`, `{"idle_timeout": null}`, `{"idle_timeout": "1ns"}`, `{"idle_timeout": "45m"}`,
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.NoError(t, check.Validate(config), text)
	}
	for _, value := range []string{"0s", "0", "-1s", "-1h30m"} {
		config, err := decodeCommandConfig(`{"idle_timeout": "` + value + `"}`)
		require.NoError(t, err, value)
		require.ErrorContains(t, check.Validate(config), "idle_timeout must be positive", value)
	}
}

// An unset or empty map is not stored, so configs without services marshal as before.
func TestCommandConfigServicesMarshal(t *testing.T) {
	for _, services := range []map[string]ServiceConfig{nil, {}} {
		config := DefaultConfig(nil)
		config.Services = services
		raw, err := json.Marshal(config)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "services")
	}
	config := DefaultConfig(nil)
	config.Services = map[string]ServiceConfig{ServiceJupyter: {}}
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"services":{"jupyter":{}}`)
}

func TestInteractiveServicesSorted(t *testing.T) {
	require.IsNonDecreasing(t, InteractiveServices)
	require.Contains(t, InteractiveServices, ServiceSSH)
	require.Contains(t, InteractiveServices, ServiceJupyter)
	require.Contains(t, InteractiveServices, ServiceVSCode)
	require.Contains(t, InteractiveServices, ServiceTensorBoard)
}

// tensorBoardSourceRequired is the error of a running tensorboard service without a data source.
const tensorBoardSourceRequired = "services: tensorboard: logdir, experiment_ids or trial_ids is required"

// The tensorboard service takes a logdir, which it requires when it runs, and args; no other
// service takes either. It is not a default service.
func TestServiceConfigTensorBoard(t *testing.T) {
	config, err := decodeCommandConfig(
		`{"services": {"tensorboard": {"logdir": "runs", "args": ["--reload_interval=5", "a b"]}}}`)
	require.NoError(t, err)
	require.NoError(t, check.Validate(config))
	require.Equal(t, ServiceConfig{LogDir: "runs", Args: []string{"--reload_interval=5", "a b"}},
		config.Services[ServiceTensorBoard])
	require.Equal(t, []string{ServiceTensorBoard}, EnabledServiceNames(config.Services))
	raw, err := json.Marshal(config.Services)
	require.NoError(t, err)
	require.JSONEq(t,
		`{"tensorboard": {"logdir": "runs", "args": ["--reload_interval=5", "a b"]}}`, string(raw))

	for _, text := range []string{
		`{"services": {"tensorboard": {"enabled": false}}}`,
		`{"services": {"tensorboard": {"enabled": false, "args": ["--x"]}}}`,
		`{"services": {"tensorboard": {"logdir": "/abs/dir"}}}`,
		`{"services": {"tensorboard": {"logdir": "runs", "args": ["--logdir_spec=a:runs", "--hostx"]}}}`,
		`{"services": {"tensorboard": {"logdir": "runs", "args": ["--reload_interval", "5"]}}}`,
		`{"services": {"tensorboard": {"enabled": false, "args": ["--load_fast=true"]}}}`,
		`{"services": {"tensorboard": {"logdir": "runs", "args": ["--", "-", "--=x", "--samples_per_plugin=scalars=500"]}}}`,
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.NoError(t, check.Validate(config), text)
	}

	for text, want := range map[string]string{
		`{"services": {"tensorboard": {}}}`:                    tensorBoardSourceRequired,
		`{"services": {"tensorboard": null}}`:                  tensorBoardSourceRequired,
		`{"services": {"tensorboard": {"enabled": true}}}`:     tensorBoardSourceRequired,
		`{"services": {"tensorboard": {"args": ["--x"]}}}`:     tensorBoardSourceRequired,
		`{"services": {"jupyter": {"logdir": "runs"}}}`:        "services: jupyter: only tensorboard takes logdir",
		`{"services": {"vscode": {"args": []}}}`:               "services: vscode: only tensorboard takes args",
		`{"services": {"ssh": {"args": ["--x"]}}}`:             "services: ssh: only tensorboard takes args",
		`{"services": {"tensorboard": {"logdir": "a\u0000"}}}`: "services: tensorboard: logdir contains a NUL",
		`{"services": {"tensorboard": {"logdir": "runs", "args": ["\u0000"]}}}`: "services: tensorboard: " +
			"args contain a NUL",
		`{"services": {"tensorboard": {"logdir": "runs", "idle_type": "activity"}}}`: "services: " +
			"tensorboard: only jupyter takes idle_type",
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.ErrorContains(t, check.Validate(config), want, text)
	}

	// The flags that the service sets, as "--flag value", as "--flag=value" or bare last, are
	// refused, whether or not the service runs, each by name.
	for _, flag := range []string{
		"--logdir", "--port", "--path_prefix", "--host", "--bind_all", "--reuse_port",
	} {
		for _, args := range []string{
			`["` + flag + `", "x"]`, `["--reload_interval=5", "` + flag + `=x"]`, `["` + flag + `"]`,
		} {
			for _, entry := range []string{`"logdir": "runs"`, `"enabled": false`} {
				text := `{"services": {"tensorboard": {` + entry + `, "args": ` + args + `}}}`
				config, err := decodeCommandConfig(text)
				require.NoError(t, err, text)
				require.ErrorContains(t, check.Validate(config),
					"services: tensorboard: args: "+flag+" is set by the service", text)
			}
		}
	}
	// So are their abbreviations, which TensorBoard takes too, each by the flag's full name, and a
	// flag file, which could hold them.
	for arg, want := range map[string]string{
		`"--bind"`: "--bind_all is set by the service", `"--hos=0.0.0.0"`: "--host is set by the service",
		`"--po", "7000"`: "--port is set by the service", `"--path_p=/x"`: "--path_prefix is set by the service",
		`"--logd"`: "--logdir is set by the service", `"--flagfile=f"`: "--flagfile is not allowed",
		`"--flagfile", "f"`: "--flagfile is not allowed", `"-flagfile=f"`: "--flagfile is not allowed",
		`"--flagf=f"`: "--flagfile is not allowed", `"--reu=true"`: "--reuse_port is set by the service",
	} {
		text := `{"services": {"tensorboard": {"logdir": "runs", "args": ["--reload_interval=5", ` + arg + `]}}}`
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.ErrorContains(t, check.Validate(config), "services: tensorboard: args: "+want, text)
	}
	text := `{"services": {"tensorboard": {"logdir": "runs", "args": ["--host=0.0.0.0", "--bind_all"]}}}`
	config, err = decodeCommandConfig(text)
	require.NoError(t, err)
	err = check.Validate(config)
	require.ErrorContains(t, err, "services: tensorboard: args: --host is set by the service")
	require.ErrorContains(t, err, "services: tensorboard: args: --bind_all is set by the service")

	require.NotContains(t, DefaultShellServices(true), ServiceTensorBoard)
	require.NotContains(t, DefaultShellServices(false), ServiceTensorBoard)
}

// A request's entry over a template's: the request's enabled wins, unset meaning enabled; each
// option that the request leaves unset is the template's; an explicit empty args list clears the
// template's. The template's other entries stay.
func TestMergeServices(t *testing.T) {
	template := map[string]ServiceConfig{
		ServiceJupyter:     {IdleType: JupyterIdleTypeKernelConnections},
		ServiceTensorBoard: {Enabled: ptrs.Ptr(false), LogDir: "runs", Args: []string{"--a"}},
		ServiceVSCode:      {Enabled: ptrs.Ptr(false)},
	}
	for _, tc := range []struct {
		name    string
		request map[string]ServiceConfig
		want    map[string]ServiceConfig
	}{
		{name: "no request", want: template},
		{
			name:    "empty entries",
			request: map[string]ServiceConfig{ServiceJupyter: {}, ServiceTensorBoard: {}},
			want: map[string]ServiceConfig{
				ServiceJupyter:     {IdleType: JupyterIdleTypeKernelConnections},
				ServiceTensorBoard: {LogDir: "runs", Args: []string{"--a"}},
				ServiceVSCode:      {Enabled: ptrs.Ptr(false)},
			},
		},
		{
			name: "options set",
			request: map[string]ServiceConfig{
				ServiceJupyter:     {IdleType: JupyterIdleTypeActivity},
				ServiceTensorBoard: {Enabled: ptrs.Ptr(true), LogDir: "/logs", Args: []string{"--b"}},
			},
			want: map[string]ServiceConfig{
				ServiceJupyter:     {IdleType: JupyterIdleTypeActivity},
				ServiceTensorBoard: {Enabled: ptrs.Ptr(true), LogDir: "/logs", Args: []string{"--b"}},
				ServiceVSCode:      {Enabled: ptrs.Ptr(false)},
			},
		},
		{
			name: "disabled kept, args cleared",
			request: map[string]ServiceConfig{
				ServiceTensorBoard: {Enabled: ptrs.Ptr(false), Args: []string{}},
				ServiceVSCode:      {Enabled: ptrs.Ptr(false)},
			},
			want: map[string]ServiceConfig{
				ServiceJupyter:     {IdleType: JupyterIdleTypeKernelConnections},
				ServiceTensorBoard: {Enabled: ptrs.Ptr(false), LogDir: "runs", Args: []string{}},
				ServiceVSCode:      {Enabled: ptrs.Ptr(false)},
			},
		},
		{
			name:    "new entry",
			request: map[string]ServiceConfig{"emacs": {}},
			want: map[string]ServiceConfig{
				ServiceJupyter:     {IdleType: JupyterIdleTypeKernelConnections},
				ServiceTensorBoard: {Enabled: ptrs.Ptr(false), LogDir: "runs", Args: []string{"--a"}},
				ServiceVSCode:      {Enabled: ptrs.Ptr(false)},
				"emacs":            {},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, MergeServices(template, tc.request))
		})
	}
	require.Nil(t, MergeServices(nil, nil))
	require.Equal(t, map[string]ServiceConfig{ServiceJupyter: {}},
		MergeServices(nil, map[string]ServiceConfig{ServiceJupyter: {}}))

	// The merge does not share the template's args.
	merged := MergeServiceConfig(template[ServiceTensorBoard], ServiceConfig{})
	merged.Args[0] = "--changed"
	require.Equal(t, "--a", template[ServiceTensorBoard].Args[0])
}

// The tensorboard service reads experiments and trials instead of a logdir: either is required
// when it runs, and they exclude each other. The IDs are positive, at most tensorBoardMaxSources of
// them, and checked whether or not the service runs; no other service takes them.
func TestTensorBoardSourcesValidation(t *testing.T) {
	for _, text := range []string{
		`{"services": {"tensorboard": {"experiment_ids": [12]}}}`,
		`{"services": {"tensorboard": {"trial_ids": [40]}}}`,
		`{"services": {"tensorboard": {"experiment_ids": [12, 12], "trial_ids": [40, 41]}}}`,
		`{"services": {"tensorboard": {"enabled": false, "experiment_ids": [12]}}}`,
		`{"services": {"tensorboard": {"experiment_ids": [12], "args": ["--reload_interval=5"]}}}`,
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.NoError(t, check.Validate(config), text)
	}

	config, err := decodeCommandConfig(
		`{"services": {"tensorboard": {"experiment_ids": [12, 13], "trial_ids": [40]}}}`)
	require.NoError(t, err)
	service := config.Services[ServiceTensorBoard]
	require.Equal(t, ServiceConfig{ExperimentIDs: []int32{12, 13}, TrialIDs: []int32{40}}, service)
	require.True(t, service.HasSources())
	require.False(t, ServiceConfig{LogDir: "runs"}.HasSources())
	require.False(t, ServiceConfig{ExperimentIDs: []int32{}, TrialIDs: []int32{}}.HasSources())
	raw, err := json.Marshal(config.Services)
	require.NoError(t, err)
	require.JSONEq(t, `{"tensorboard": {"experiment_ids": [12, 13], "trial_ids": [40]}}`, string(raw))

	for text, want := range map[string]string{
		`{"services": {"tensorboard": {"experiment_ids": []}}}`:                  tensorBoardSourceRequired,
		`{"services": {"tensorboard": {"experiment_ids": [], "trial_ids": []}}}`: tensorBoardSourceRequired,
		`{"services": {"tensorboard": {"logdir": "runs", "experiment_ids": [12]}}}`: "services: " +
			"tensorboard: logdir excludes experiment_ids and trial_ids",
		`{"services": {"tensorboard": {"logdir": "runs", "trial_ids": [40]}}}`: "services: " +
			"tensorboard: logdir excludes experiment_ids and trial_ids",
		`{"services": {"tensorboard": {"experiment_ids": [12, 0, -1]}}}`: "services: tensorboard: " +
			"experiment_ids: 0 is not an ID",
		`{"services": {"tensorboard": {"trial_ids": [-3]}}}`: "services: tensorboard: " +
			"trial_ids: -3 is not an ID",
		`{"services": {"tensorboard": {"enabled": false, "experiment_ids": [0]}}}`: "services: " +
			"tensorboard: experiment_ids: 0 is not an ID",
		`{"services": {"tensorboard": {"enabled": false, "trial_ids": [-1]}}}`: "services: " +
			"tensorboard: trial_ids: -1 is not an ID",
		`{"services": {"jupyter": {"experiment_ids": [12]}}}`: "services: jupyter: only tensorboard " +
			"takes experiment_ids",
		`{"services": {"vscode": {"trial_ids": []}}}`: "services: vscode: only tensorboard takes trial_ids",
		`{"services": {"ssh": {"trial_ids": [40]}}}`:  "services: ssh: only tensorboard takes trial_ids",
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.ErrorContains(t, check.Validate(config), want, text)
	}

	// Only the first bad ID of each list is reported.
	config, err = decodeCommandConfig(
		`{"services": {"tensorboard": {"experiment_ids": [0, -1], "trial_ids": [5, -2, 0]}}}`)
	require.NoError(t, err)
	err = check.Validate(config)
	require.ErrorContains(t, err, "services: tensorboard: experiment_ids: 0 is not an ID")
	require.ErrorContains(t, err, "services: tensorboard: trial_ids: -2 is not an ID")
	require.NotContains(t, err.Error(), "-1 is not an ID")
	require.NotContains(t, err.Error(), "trial_ids: 0 is not an ID")

	// The cap counts both lists together, before deduplication.
	ids := func(n int) []int32 {
		out := make([]int32, n)
		for i := range out {
			out[i] = 1
		}
		return out
	}
	const tooMany = "services: tensorboard: at most 1000 experiment_ids and trial_ids"
	for _, tc := range []struct {
		exps, trials int
		ok           bool
	}{
		{exps: 1000, ok: true},
		{exps: 600, trials: 400, ok: true},
		{trials: 1000, ok: true},
		{exps: 1001},
		{exps: 600, trials: 401},
		{trials: 1001},
	} {
		config := DefaultConfig(nil)
		config.Entrypoint = []string{"run"}
		config.Services = map[string]ServiceConfig{
			ServiceTensorBoard: {ExperimentIDs: ids(tc.exps), TrialIDs: ids(tc.trials)},
		}
		if tc.ok {
			require.NoError(t, check.Validate(config), tc)
		} else {
			require.ErrorContains(t, check.Validate(config), tooMany, tc)
		}
	}
}

// The logdir, experiment_ids and trial_ids of a tensorboard entry merge as one data source: a
// request that sets any of them replaces the template's, an explicit empty list included, and one
// that sets none keeps the template's.
func TestMergeServiceConfigSource(t *testing.T) {
	for _, tc := range []struct {
		name              string
		template, request ServiceConfig
		want              ServiceConfig
	}{
		{
			name:     "request ids over template logdir",
			template: ServiceConfig{LogDir: "runs", Args: []string{"--a"}},
			request:  ServiceConfig{ExperimentIDs: []int32{12}},
			want:     ServiceConfig{ExperimentIDs: []int32{12}, Args: []string{"--a"}},
		},
		{
			name:     "request trial ids over template experiment ids",
			template: ServiceConfig{ExperimentIDs: []int32{12}},
			request:  ServiceConfig{TrialIDs: []int32{40}},
			want:     ServiceConfig{TrialIDs: []int32{40}},
		},
		{
			name:     "request logdir over template ids",
			template: ServiceConfig{ExperimentIDs: []int32{12}, TrialIDs: []int32{40}},
			request:  ServiceConfig{LogDir: "runs"},
			want:     ServiceConfig{LogDir: "runs"},
		},
		{
			name:     "empty request keeps template ids",
			template: ServiceConfig{ExperimentIDs: []int32{12}, TrialIDs: []int32{40}},
			want:     ServiceConfig{ExperimentIDs: []int32{12}, TrialIDs: []int32{40}},
		},
		{
			name:     "empty request keeps template logdir",
			template: ServiceConfig{LogDir: "runs"},
			want:     ServiceConfig{LogDir: "runs"},
		},
		{
			name:     "explicit empty list clears",
			template: ServiceConfig{LogDir: "runs", ExperimentIDs: []int32{12}, TrialIDs: []int32{40}},
			request:  ServiceConfig{ExperimentIDs: []int32{}},
			want:     ServiceConfig{ExperimentIDs: []int32{}},
		},
		{
			name:     "disabled kept",
			template: ServiceConfig{ExperimentIDs: []int32{12}},
			request:  ServiceConfig{Enabled: ptrs.Ptr(false)},
			want:     ServiceConfig{Enabled: ptrs.Ptr(false), ExperimentIDs: []int32{12}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, MergeServiceConfig(tc.template, tc.request))
		})
	}

	// A request's `tensorboard: {}` decodes to an entry that sets no source, and an explicit []
	// decodes to an empty list that is set.
	var request map[string]ServiceConfig
	require.NoError(t, json.Unmarshal(
		[]byte(`{"tensorboard": {}, "jupyter": {"experiment_ids": []}}`), &request))
	require.Nil(t, request[ServiceTensorBoard].ExperimentIDs)
	require.NotNil(t, request[ServiceJupyter].ExperimentIDs)

	// The merge does not share the template's IDs.
	template := ServiceConfig{ExperimentIDs: []int32{12}, TrialIDs: []int32{40}}
	merged := MergeServiceConfig(template, ServiceConfig{})
	merged.ExperimentIDs[0] = 13
	merged.TrialIDs[0] = 41
	require.Equal(t, []int32{12}, template.ExperimentIDs)
	require.Equal(t, []int32{40}, template.TrialIDs)
}

// A shell runs without its SSH server only to view TensorBoard logs: its tensorboard service runs
// with experiment_ids or trial_ids, and it takes no slots and no gpu_groups. Such a shell's
// tensorboard service is its primary one.
func TestSSHDisabledShape(t *testing.T) {
	const viewer = `"ssh": {"enabled": false}, "tensorboard": {"experiment_ids": [12], "trial_ids": [40]}`
	for _, text := range []string{
		`{"services": {` + viewer + `}}`,
		`{"services": {` + viewer + `}, "resources": {"slots": 0}}`,
		`{"services": {` + viewer + `, "jupyter": {}}, "resources": {"slots": 0}}`,
		`{"services": {"ssh": {"enabled": false}, "tensorboard": {"trial_ids": [40]}}}`,
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.NoError(t, check.Validate(config), text)
		require.False(t, ShellRunsSSH(config.Services), text)
		require.True(t, config.TensorBoardPrimary(), text)
	}

	const onlyForSources = "services: ssh: can be disabled only for tensorboard with experiment_ids " +
		"or trial_ids"
	for text, want := range map[string]string{
		`{"services": {` + viewer + `}, "resources": {"slots": 1}}`: "services: ssh: can be " +
			"disabled only with resources.slots 0",
		`{"services": {` + viewer + `}, "resources": {"gpu_groups": [{"name": "a", "count": 1}]}}`: "" +
			"services: ssh: can be disabled only without resources.gpu_groups",
		`{"services": {"ssh": {"enabled": false}, "tensorboard": {"logdir": "runs"}}}`: onlyForSources,
		`{"services": {"ssh": {"enabled": false}, "tensorboard": {"enabled": false, ` +
			`"experiment_ids": [12]}}}`: onlyForSources,
		`{"services": {"ssh": {"enabled": false}, "jupyter": {}}}`: onlyForSources,
		`{"services": {"ssh": {"enabled": false}}}`:                onlyForSources,
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.ErrorContains(t, check.Validate(config), want, text)
	}

	// A shell that runs ssh stays an ordinary shell, with or without slots, and its tensorboard
	// service may read experiments too.
	for _, text := range []string{
		`{"resources": {"slots": 0}}`,
		`{"services": {"ssh": {}}, "resources": {"slots": 0}}`,
		`{"services": {"ssh": {"enabled": true}, "tensorboard": {"experiment_ids": [12]}}, ` +
			`"resources": {"slots": 2}}`,
		`{"services": {"tensorboard": {"experiment_ids": [12]}}, "resources": {"slots": 1}}`,
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.NoError(t, check.Validate(config), text)
		require.True(t, ShellRunsSSH(config.Services), text)
		require.False(t, config.TensorBoardPrimary(), text)
	}
	require.True(t, ShellRunsSSH(nil))
}
