package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/experimentv1"
)

func configWithRegistryAuth(t *testing.T) *structpb.Struct {
	config, err := structpb.NewStruct(map[string]any{
		"description": "d",
		"environment": map[string]any{
			"image":         "registry.example.com/private:1",
			"registry_auth": map[string]any{"username": "u", "password": "p"},
		},
	})
	require.NoError(t, err)
	return config
}

func TestRedactTaskConfig(t *testing.T) {
	const ownerID = 7
	owner := model.User{ID: ownerID}
	admin := model.User{ID: 1, Admin: true}
	other := model.User{ID: 8}

	for _, c := range []struct {
		name     string
		user     model.User
		ownerID  int32
		seesAuth bool
	}{
		{"owner", owner, ownerID, true},
		{"admin", admin, ownerID, true},
		{"another user", other, ownerID, false},
		{"another user, task without owner", other, 0, false},
		{"admin, task without owner", admin, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			config := configWithRegistryAuth(t)
			redactTaskConfig(c.user, c.ownerID, "task", config)
			env := config.Fields["environment"].GetStructValue()
			require.Equal(t, c.seesAuth, hasRegistryAuth(config))
			_, has := env.Fields[registryAuthKey]
			require.Equal(t, c.seesAuth, has)
			require.Equal(t, "registry.example.com/private:1", env.Fields["image"].GetStringValue())
			require.Equal(t, "d", config.Fields["description"].GetStringValue())
		})
	}

	// Configs without an environment, or without credentials, are left alone.
	for _, config := range []*structpb.Struct{
		nil,
		{},
		{Fields: map[string]*structpb.Value{"environment": structpb.NewNullValue()}},
		{Fields: map[string]*structpb.Value{"environment": structpb.NewStringValue("x")}},
	} {
		require.NotPanics(t, func() { redactTaskConfig(other, ownerID, "task", config) })
		require.False(t, hasRegistryAuth(config))
	}

	nullAuth, err := structpb.NewStruct(map[string]any{
		"environment": map[string]any{"registry_auth": nil},
	})
	require.NoError(t, err)
	require.False(t, hasRegistryAuth(nullAuth))
}

func TestRemoveRegistryAuthText(t *testing.T) {
	const redacted = `{"environment":{"image":"i"},"name":"exp"}`
	for _, c := range []struct {
		name, in, want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "blank", in: "  \n", want: ""},
		// A config is always returned as the master parsed it, with or without credentials.
		{
			name: "JSON without credentials",
			in:   `{"name": "exp", "environment": {"image": "i"}}`,
			want: redacted,
		},
		{name: "YAML without credentials", in: "name: exp\nenvironment:\n  image: i\n", want: redacted},
		{name: "YAML without environment", in: "name: exp\n", want: `{"name":"exp"}`},
		{
			name: "JSON",
			in:   `{"name": "exp", "environment": {"image": "i", "registry_auth": {"password": "p"}}}`,
			want: redacted,
		},
		{
			name: "YAML",
			in:   "name: exp\nenvironment:\n  image: i\n  registry_auth:\n    password: p\n",
			want: redacted,
		},
		{
			name: "null",
			in:   `{"environment": {"registry_auth": null}}`,
			want: `{"environment":{}}`,
		},
		// The master accepts YAML through schemas.JSONFromYaml (ghodss/yaml on yaml.v2), and these
		// configs must parse the same way here.
		{
			name: "YAML with a duplicate key and without credentials",
			in:   "name: exp\ndescription: first\ndescription: second\n",
			want: `{"description":"second","name":"exp"}`,
		},
		{
			name: "YAML with a duplicate key, the last one wins",
			in:   "name: x\nname: exp\nenvironment:\n  image: i\n  registry_auth:\n    password: p\n",
			want: redacted,
		},
		{
			name: "YAML with integer mapping keys",
			in: "name: exp\ndata:\n  label_map:\n    0: cat\n    1: dog\n" +
				"environment:\n  image: i\n  registry_auth:\n    password: p\n",
			want: `{"data":{"label_map":{"0":"cat","1":"dog"}},"environment":{"image":"i"},"name":"exp"}`,
		},
		{
			name: "YAML 1.1 booleans",
			in:   "name: exp\ndebug: yes\nenvironment:\n  image: i\n  registry_auth:\n    password: p\n",
			want: `{"debug":true,"environment":{"image":"i"},"name":"exp"}`,
		},
		// Credentials that the master did not read stay in the submitted text, and must not come
		// back from it.
		{
			name: "YAML with credentials in an environment that a later one replaces",
			in: "name: exp\nenvironment:\n  registry_auth:\n    password: p\n" +
				"environment:\n  image: i\n",
			want: redacted,
		},
		{
			name: "YAML flow mappings with credentials in an environment that a later one replaces",
			in: "name: exp\nenvironment: {registry_auth: {password: p}}\n" +
				"environment: {image: i}\n",
			want: redacted,
		},
		{
			name: "JSON with credentials in an environment that a later one replaces",
			in: `{"name": "exp", "environment": {"registry_auth": {"password": "p"}}, ` +
				`"environment": {"image": "i"}}`,
			want: redacted,
		},
		{
			name: "YAML with credentials that a later null replaces",
			in: "name: exp\nenvironment:\n  image: i\n  registry_auth:\n    password: p\n" +
				"  registry_auth: null\n",
			want: redacted,
		},
		{
			name: "JSON with credentials that a later null replaces",
			in: `{"name": "exp", "environment": {"image": "i", ` +
				`"registry_auth": {"password": "p"}, "registry_auth": null}}`,
			want: redacted,
		},
		{
			name: "YAML with credentials in a comment",
			in:   "name: exp\nenvironment:\n  image: i\n  # registry_auth: {password: p}\n",
			want: redacted,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := removeRegistryAuthText(c.in)
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}

	_, err := removeRegistryAuthText("environment: [unterminated")
	require.Error(t, err, "a config that does not parse is not returned to another user")
}

func TestRedactTaskConfigText(t *testing.T) {
	const config = `{"environment": {"registry_auth": {"username": "u", "password": "p"}}}`
	owner := model.User{ID: 7}
	admin := model.User{ID: 1, Admin: true}
	other := model.User{ID: 8}

	for _, u := range []model.User{owner, admin} {
		got, err := redactTaskConfigText(u, 7, "task", config)
		require.NoError(t, err)
		require.Equal(t, config, got)
	}
	got, err := redactTaskConfigText(other, 7, "task", config)
	require.NoError(t, err)
	require.Equal(t, `{"environment":{}}`, got)

	// The owner and admins get a config that does not parse as it is; anyone else gets an error.
	const broken = "environment: [unterminated"
	got, err = redactTaskConfigText(admin, 7, "task", broken)
	require.NoError(t, err)
	require.Equal(t, broken, got)
	_, err = redactTaskConfigText(other, 7, "task", broken)
	require.Error(t, err)
}

func TestRedactExperimentRegistryAuth(t *testing.T) {
	const original = "name: exp\nenvironment:\n  registry_auth:\n    password: p\n"
	newExp := func() *experimentv1.Experiment {
		return &experimentv1.Experiment{
			Id: 3, UserId: 7, Config: configWithRegistryAuth(t), OriginalConfig: original,
		}
	}

	for _, u := range []model.User{{ID: 7}, {ID: 1, Admin: true}} {
		exp := newExp()
		require.NoError(t, redactExperimentRegistryAuth(u, exp))
		require.True(t, hasRegistryAuth(exp.Config)) //nolint:staticcheck
		require.Equal(t, original, exp.OriginalConfig)
	}

	exp := newExp()
	require.NoError(t, redactExperimentRegistryAuth(model.User{ID: 8}, exp))
	require.False(t, hasRegistryAuth(exp.Config)) //nolint:staticcheck
	require.Equal(t, `{"environment":{},"name":"exp"}`, exp.OriginalConfig)

	// An experiment without an original config, as in the experiment lists.
	exp = newExp()
	exp.OriginalConfig = ""
	require.NoError(t, redactExperimentRegistryAuth(model.User{ID: 8}, exp))
	require.False(t, hasRegistryAuth(exp.Config)) //nolint:staticcheck
	require.Empty(t, exp.OriginalConfig)

	// Without credentials in its config, which the master parsed from the original config, an
	// experiment's original config can still hold credentials that a later key replaced. Another
	// user gets the original config as the master parsed it, whatever the config holds.
	for original, want := range map[string]string{
		"name: exp\ndescription: first\ndescription: second\n": `{"description":"second","name":"exp"}`,
		"name: exp\nenvironment:\n  registry_auth:\n    password: p\n" +
			"environment:\n  image: i\n": `{"environment":{"image":"i"},"name":"exp"}`,
		"name: exp\nenvironment:\n  image: i\n  registry_auth:\n    password: p\n" +
			"  registry_auth: null\n": `{"environment":{"image":"i"},"name":"exp"}`,
	} {
		exp = newExp()
		removeRegistryAuth(exp.Config) //nolint:staticcheck
		exp.OriginalConfig = original
		require.NoError(t, redactExperimentRegistryAuth(model.User{ID: 8}, exp))
		require.Equal(t, want, exp.OriginalConfig)
	}

	// An original config that does not parse fails another user's read, with or without
	// credentials in the config.
	for _, withAuth := range []bool{true, false} {
		exp = newExp()
		if !withAuth {
			removeRegistryAuth(exp.Config) //nolint:staticcheck
		}
		exp.OriginalConfig = "environment: [unterminated"
		require.Error(t, redactExperimentRegistryAuth(model.User{ID: 8}, exp))
	}

	// With credentials, integer mapping keys elsewhere in the original config do not stop them
	// from being removed.
	exp = newExp()
	exp.OriginalConfig = "name: exp\ndata:\n  splits:\n    0: train\n" +
		"environment:\n  registry_auth:\n    password: p\n"
	require.NoError(t, redactExperimentRegistryAuth(model.User{ID: 8}, exp))
	require.Equal(t, `{"data":{"splits":{"0":"train"}},"environment":{},"name":"exp"}`,
		exp.OriginalConfig)
}
