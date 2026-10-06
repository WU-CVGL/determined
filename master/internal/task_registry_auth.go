package internal

import (
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/proto/pkg/experimentv1"
)

// A task's or an experiment's environment.registry_auth holds its owner's container registry
// credentials. The API returns them only to the owner and to admins (canReadTaskCredential), who
// may need them to launch the task again. Every other reader gets the config without the key.
//
// The key is removed rather than replaced with the "********" placeholder used for cluster and pool
// configs: configs read through the API are launched again (generic task forks, experiment forks
// in the WebUI), and a placeholder would be sent to the registry as a password. Without the key,
// such a launch by another user falls back to the registry_auth of the task container defaults,
// as a TensorBoard on another user's experiment does.
//
// These helpers change only the copy of a config that goes into an API response, never the config
// that a task runs with.

const registryAuthKey = "registry_auth"

// removeRegistryAuth deletes environment.registry_auth from a config.
func removeRegistryAuth(config *structpb.Struct) {
	if env := config.GetFields()["environment"].GetStructValue(); env != nil {
		delete(env.Fields, registryAuthKey)
	}
}

// hasRegistryAuth reports whether a config sets environment.registry_auth to something other than
// null.
func hasRegistryAuth(config *structpb.Struct) bool {
	auth, ok := config.GetFields()["environment"].GetStructValue().GetFields()[registryAuthKey]
	if !ok {
		return false
	}
	_, isNull := auth.GetKind().(*structpb.Value_NullValue)
	return !isNull
}

// redactTaskConfig removes environment.registry_auth from the config in an API response unless
// user owns the task or is an admin. It logs an admin's read of another user's credentials.
func redactTaskConfig(user model.User, ownerID int32, taskID string, config *structpb.Struct) {
	if !canReadTaskCredential(user, ownerID) {
		removeRegistryAuth(config)
		return
	}
	if hasRegistryAuth(config) {
		logCredentialRead(user, registryAuthKey, taskID, ownerID)
	}
}

// parseTextConfig parses a JSON or YAML config and returns it with its environment section, if it
// has one. YAML is read as the master read it when it accepted the config (schemas.JSONFromYaml),
// so that every stored config parses and means the same: the last of duplicate keys wins, mapping
// keys become strings, and YAML 1.1 booleans such as "yes" are booleans.
func parseTextConfig(config string) (map[string]interface{}, map[string]interface{}, error) {
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(config), &parsed); err != nil {
		converted, err := schemas.JSONFromYaml([]byte(config))
		if err != nil {
			return nil, nil, fmt.Errorf("parsing config: %w", err)
		}
		parsed = nil
		if err := json.Unmarshal(converted, &parsed); err != nil {
			return nil, nil, fmt.Errorf("parsing config: %w", err)
		}
	}
	env, _ := parsed["environment"].(map[string]interface{})
	return parsed, env, nil
}

// removeRegistryAuthText returns a JSON or YAML config as JSON, without environment.registry_auth.
// It always encodes the config it parsed, never the text it was given: the master's parser keeps
// the last of duplicate keys, while the database keeps the text as submitted, so the text can hold
// credentials that the parsed config does not. Examples are a registry_auth in an environment
// section that a later environment section replaces, a registry_auth that a later
// "registry_auth: null" replaces, and credentials in YAML comments. A blank config comes back
// empty. A config that does not parse is an error: there is no telling what it holds.
func removeRegistryAuthText(config string) (string, error) {
	if strings.TrimSpace(config) == "" {
		return "", nil
	}
	parsed, env, err := parseTextConfig(config)
	if err != nil {
		return "", err
	}
	delete(env, registryAuthKey)
	out, err := json.Marshal(parsed)
	if err != nil {
		return "", fmt.Errorf("encoding config: %w", err)
	}
	return string(out), nil
}

// redactTaskConfigText is redactTaskConfig for a config stored as JSON or YAML text.
func redactTaskConfigText(user model.User, ownerID int32, taskID, config string) (string, error) {
	if !canReadTaskCredential(user, ownerID) {
		out, err := removeRegistryAuthText(config)
		if err != nil {
			return "", fmt.Errorf("removing registry credentials from the config of task %s: %w",
				taskID, err)
		}
		return out, nil
	}
	if user.ID != model.UserID(ownerID) && strings.TrimSpace(config) != "" {
		if _, env, err := parseTextConfig(config); err == nil && env[registryAuthKey] != nil {
			logCredentialRead(user, registryAuthKey, taskID, ownerID)
		}
	}
	return config, nil
}

// redactExperimentRegistryAuth removes environment.registry_auth from an experiment's config and
// original config unless user owns the experiment or is an admin; they get the original config as
// it was submitted. Anyone else gets the original config from removeRegistryAuthText, as JSON, even
// when the config has no registry_auth: the original text can hold credentials that the config,
// which the master parsed from it, does not. For them, an original config that does not parse
// fails the read; the master parsed every original config with this parser when it accepted it.
// Experiments are read often, by the WebUI's polling among others, so an admin's reads are not
// logged.
func redactExperimentRegistryAuth(user model.User, exp *experimentv1.Experiment) error {
	if canReadTaskCredential(user, exp.UserId) {
		return nil
	}
	removeRegistryAuth(exp.Config) //nolint:staticcheck
	out, err := removeRegistryAuthText(exp.OriginalConfig)
	if err != nil {
		return fmt.Errorf(
			"removing registry credentials from the original config of experiment %d: %w",
			exp.Id, err)
	}
	exp.OriginalConfig = out
	return nil
}
