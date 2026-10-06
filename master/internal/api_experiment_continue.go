package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"github.com/uptrace/bun"
	"golang.org/x/exp/maps"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/configpolicy"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/command"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/trialv1"
)

// continueConfig is a continue's override config merged into the experiment's active config.
type continueConfig struct {
	config   []byte
	isSingle bool
	// ownerOnlyChanges are the top-level fields that continueOwnerOnlyChanges reports the override
	// to change.
	ownerOnlyChanges []string
}

func (a *apiServer) parseAndMergeContinueConfig(expID int, overrideConfig string) (
	*continueConfig, error,
) {
	if overrideConfig == "" {
		overrideConfig = "{}" //nolint: goconst
	}

	activeConfig, err := a.m.db.ActiveExperimentConfig(expID)
	if err != nil {
		return nil, fmt.Errorf("loading active config for experiment %d: %w", expID, err)
	}
	name := activeConfig.Searcher().AsLegacy().Name
	isSingle := name == "single"                           //nolint: goconst
	if !isSingle && (name != "grid" && name != "random") { //nolint: goconst
		return nil, status.Errorf(codes.InvalidArgument,
			fmt.Sprintf("Unsupported searcher type provided: '%s'", name))
	}
	if !isSingle && strings.TrimSpace(overrideConfig) != "{}" { //nolint: goconst
		return nil, status.Errorf(codes.InvalidArgument,
			fmt.Sprintf("override config is provided and experiment is not single searcher, got '%s' instead", name))
	}

	providedConfig, err := expconf.ParseAnyExperimentConfigYAML([]byte(overrideConfig))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			fmt.Errorf("parsing override config: %w", err).Error())
	}

	if providedConfig.RawProject != nil {
		return nil, status.Errorf(codes.InvalidArgument, "'project' in override config "+
			"cannot be specified, use `det experiment move` first if you want to change the project")
	}
	if providedConfig.RawWorkspace != nil {
		return nil, status.Errorf(codes.InvalidArgument, "'workspace' in override config "+
			"cannot be specified, use `det experiment move` first if you want to change the workspace")
	}
	mergedConfig := schemas.Merge(providedConfig, activeConfig)
	if overrideName := mergedConfig.Searcher().AsLegacy().Name; isSingle && overrideName != "single" {
		return nil, status.Errorf(codes.InvalidArgument,
			fmt.Sprintf("override config must have single searcher type got '%s' instead", overrideName))
	}
	// Compared before the invariant configs are merged in: they are the cluster's, not the override's.
	ownerOnlyChanges, err := continueOwnerOnlyChanges(activeConfig, mergedConfig)
	if err != nil {
		return nil, fmt.Errorf("comparing the override config: %w", err)
	}

	// Merge the config with the optionally specified invariant config specified by task config
	// policies.
	w, err := getWorkspaceByConfig(activeConfig)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			fmt.Sprintf("failed to get workspace %s", activeConfig.Workspace()))
	}

	configWithInvariantDefaults, err := configpolicy.MergeWithInvariantExperimentConfigs(
		context.TODO(),
		w.ID, mergedConfig)
	if err != nil {
		return nil,
			fmt.Errorf("failed to merge invariant experiment configs: %w", err)
	}
	mergedConfig = *configWithInvariantDefaults

	bytes, err := mergedConfig.Value()
	if err != nil {
		return nil, fmt.Errorf("getting value of merged config: %w", err)
	}

	return &continueConfig{
		config: bytes.([]byte), isSingle: isSingle, ownerOnlyChanges: ownerOnlyChanges,
	}, nil
}

// continueNonOwnerFields are the config fields, as paths, that someone other than an experiment's
// owner may change when they continue it. Its trials run as the owner, so every other field stays as
// the owner set it: the ones that choose the code, the image, the mounts, the environment, the
// storage location, the resource pool, the launcher's arguments, and the hyperparameters and data
// that the code reads. These name the experiment or bound the training it already does:
//   - name, description and labels, which PatchExperiment also writes into the config, for
//     anyone with CanEditExperimentsMetadata (UPDATE_EXPERIMENT_METADATA under RBAC, which is
//     weaker than continue), and a continue without an override then uses. Resume Current Trial
//     in the WebUI prefixes the description with "Fork of". Labels also reach the launcher: when
//     its job_project_source is "label" or "label:<prefix>", they set the Slurm --wckey and the
//     PBS -P project that the owner's job is accounted to (jobAndProjectLabels in pkg/tasks), each
//     one quoted argument. They do not choose the code, and refusing them here would not stop
//     PatchExperiment.
//   - max_restarts is how many times a failed trial is restarted.
//   - searcher.max_length is how long the trial trains, for code that still reads it: legacy
//     Trial classes, and Trainer.fit without max_length. It is deprecated; where the code sets
//     the length itself, from the entrypoint or the hyperparameters, only the owner can extend
//     it. Only a single-trial experiment takes an override config.
//   - checkpoint_storage.save_* are how many checkpoints are kept, which PatchExperiment also
//     changes. Where they are stored does not change.
//
// Left out on purpose: resources, because the resource pool brings its own task container defaults
// (image, environment variables, mounts, pod spec), and priority, weight and max_slots are set on
// the running experiment, which checks the workspace's limits; min_validation_period,
// min_checkpoint_period, log_policies and debug, which no continue needs.
var continueNonOwnerFields = [][]string{
	{"name"},
	{"description"},
	{"labels"},
	{"max_restarts"},
	{"searcher", "max_length"},
	{"checkpoint_storage", "save_experiment_best"},
	{"checkpoint_storage", "save_trial_best"},
	{"checkpoint_storage", "save_trial_latest"},
}

// continueNonOwnerFieldNames returns continueNonOwnerFields for messages.
func continueNonOwnerFieldNames() string {
	names := make([]string, 0, len(continueNonOwnerFields))
	for _, path := range continueNonOwnerFields {
		names = append(names, strings.Join(path, "."))
	}
	return strings.Join(names, ", ")
}

// continueOwnerOnlyChanges returns the top-level config fields whose value in merged differs from
// active once continueNonOwnerFields are left out: what only the owner may change when continuing
// the experiment. Both configs are compared in the same form, with defaults and with environment
// variables by their effective values.
func continueOwnerOnlyChanges(active, merged expconf.ExperimentConfig) ([]string, error) {
	a, err := continueComparableConfig(active)
	if err != nil {
		return nil, fmt.Errorf("active config: %w", err)
	}
	m, err := continueComparableConfig(merged)
	if err != nil {
		return nil, fmt.Errorf("merged config: %w", err)
	}
	fields := maps.Keys(a)
	for f := range m {
		if _, ok := a[f]; !ok {
			fields = append(fields, f)
		}
	}
	sort.Strings(fields)
	var changed []string
	for _, f := range fields {
		// A field left out of one config (omitempty) and set in the other is a change.
		if a[f] != m[f] {
			changed = append(changed, f)
		}
	}
	return changed, nil
}

// continueComparableConfig returns c's top-level fields as canonical JSON, with defaults, with
// environment variables by their effective values, and without continueNonOwnerFields.
func continueComparableConfig(c expconf.ExperimentConfig) (map[string]string, error) {
	c = schemas.WithDefaults(c)
	c.RawEnvironment = effectiveEnvironment(c.RawEnvironment)
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var config map[string]any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber() // Large integers stay exact.
	if err := d.Decode(&config); err != nil {
		return nil, err
	}
	for _, path := range continueNonOwnerFields {
		parent := config
		for _, key := range path[:len(path)-1] {
			next, ok := parent[key].(map[string]any)
			if !ok {
				parent = nil
				break
			}
			parent = next
		}
		if parent != nil {
			delete(parent, path[len(path)-1])
		}
	}
	out := make(map[string]string, len(config))
	for f, v := range config {
		// Maps marshal with sorted keys.
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out[f] = string(b)
	}
	return out, nil
}

// effectiveEnvironment returns a copy of env whose environment variables keep only the last entry
// for each variable, sorted. A continue's override variables are appended to the active ones
// (EnvironmentVariablesMapV0.Merge), and the WebUI's Resume Current Trial sends back the whole
// config, so a variable repeated with the value it has is not a change.
func effectiveEnvironment(env *expconf.EnvironmentConfigV0) *expconf.EnvironmentConfigV0 {
	if env == nil || env.RawEnvironmentVariables == nil {
		return env
	}
	effective := func(vars []string) []string {
		last := map[string]string{}
		for _, v := range vars {
			name, _, _ := strings.Cut(v, "=")
			last[name] = v
		}
		out := maps.Values(last)
		sort.Strings(out)
		return out
	}
	out := schemas.Copy(*env)
	out.RawEnvironmentVariables.RawCPU = effective(out.RawEnvironmentVariables.RawCPU)
	out.RawEnvironmentVariables.RawCUDA = effective(out.RawEnvironmentVariables.RawCUDA)
	out.RawEnvironmentVariables.RawROCM = effective(out.RawEnvironmentVariables.RawROCM)
	return &out
}

var errContinueHPSearchCompleted = status.Error(codes.FailedPrecondition,
	"experiment has been completed, cannot continue this experiment")

func (a *apiServer) ContinueExperiment(
	ctx context.Context, req *apiv1.ContinueExperimentRequest,
) (*apiv1.ContinueExperimentResponse, error) {
	actor, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get the user: %s", err)
	}

	origExperiment, _, err := a.getExperimentAndCheckCanDoActions(ctx, int(req.Id),
		experiment.AuthZProvider.Get().CanEditExperiment)
	if err != nil {
		return nil, err
	}

	// The continued experiment keeps its owner: an administrator, or under RBAC another user who
	// may edit it, continues the owner's code, and its tasks run as the owner, not as them.
	if origExperiment.OwnerID == nil {
		return nil, status.Errorf(codes.Internal, "experiment %d has no owner", req.Id)
	}
	ownerFull, err := user.ByID(ctx, *origExperiment.OwnerID)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"loading the owner of experiment %d: %s", req.Id, err)
	}
	if !ownerFull.Active {
		return nil, status.Errorf(codes.FailedPrecondition,
			"experiment %d belongs to user %q, who is deactivated; its tasks run as its owner, "+
				"so reactivate the user to continue it", req.Id, ownerFull.Username)
	}
	owner := ownerFull.ToUser()

	trialsResp, err := a.GetExperimentTrials(ctx, &apiv1.GetExperimentTrialsRequest{
		ExperimentId: req.Id,
	})
	if err != nil {
		return nil, fmt.Errorf("getting experiment trials: %w", err)
	}
	merged, err := a.parseAndMergeContinueConfig(int(req.Id), req.OverrideConfig)
	if err != nil {
		return nil, err
	}
	// The trials run as the owner, so a continuer who changed what they run, or what they run it
	// with, would run their own choice with the owner's token and uid/gid. Anyone but the owner may
	// change only the fields that bound the training (continueNonOwnerFields).
	if actor.ID != owner.ID && len(merged.ownerOnlyChanges) > 0 {
		return nil, status.Errorf(codes.PermissionDenied,
			"experiment %d runs as its owner %q, so only they may change %s when continuing it; "+
				"anyone else may change only %s. To run a changed copy as yourself, fork the experiment",
			req.Id, owner.Username, strings.Join(merged.ownerOnlyChanges, ", "),
			continueNonOwnerFieldNames())
	}

	dbExp, modelDef, activeConfig, _, taskSpec, err := a.m.parseCreateExperiment(ctx,
		&apiv1.CreateExperimentRequest{
			Config: string(merged.config),
		}, actor, &owner,
	)
	if err != nil {
		return nil, fmt.Errorf("parsing continue experiment request: %w", err)
	}
	// Until the experiment starts, nothing else holds the session made for its tasks.
	started := false
	defer func() {
		if !started {
			deleteTaskSessionToken(taskSpec.UserSessionToken)
		}
	}()
	dbExp.ID = int(req.Id)
	dbExp.JobID = origExperiment.JobID // Revive job.

	e, launchWarnings, err := newExperiment(a.m, dbExp, modelDef, activeConfig, taskSpec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create experiment: %s", err)
	}

	err = db.Bun().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Lock experiment state.
		var expState model.State
		if err = tx.
			NewRaw(`SELECT state FROM experiments WHERE id = ? FOR UPDATE`, req.Id).
			Scan(ctx, &expState); err != nil {
			return fmt.Errorf("getting / locking experiment state: %w", err)
		}
		if !model.TerminalStates[expState] {
			return status.Error(codes.FailedPrecondition, fmt.Sprintf(
				"experiment in non terminal state '%s', try again later", expState))
		}

		if expState == model.CompletedState && !merged.isSingle {
			hasIncompleteTrials := false
			for _, trial := range trialsResp.Trials {
				if trial.State != trialv1.State_STATE_COMPLETED {
					hasIncompleteTrials = true
					break
				}
			}
			if !hasIncompleteTrials {
				return errContinueHPSearchCompleted
			}
		} else if merged.isSingle && len(trialsResp.Trials) > 0 {
			if _, err := tx.NewUpdate().Table("runs"). // TODO(nick-runs) call runs package.
									Set("state = ?", model.PausedState).
									Where("id = ?", trialsResp.Trials[0].Id).
									Exec(ctx); err != nil {
				return fmt.Errorf("changing trial state to PAUSED: %w", err)
			}
		}

		if _, err := tx.NewUpdate().Model(&model.Experiment{}).
			Set("state = ?", model.PausedState). // Throw it in paused.
			Set("progress = ?", 0.0).            // Reset progress.
			Set("end_time = null").
			Where("id = ?", req.Id).
			Exec(ctx); err != nil {
			return fmt.Errorf("updating experiments config: %w", err)
		}

		if _, err := db.Bun().NewUpdate().Model(&model.Job{}).
			Set("q_position = DEFAULT").
			Where("job_id = ?", dbExp.JobID).
			Exec(ctx); err != nil {
			return fmt.Errorf("updating experiment's job: %w", err)
		}

		// Update active config but not original config.
		// We actually do this in experiment's PreStart in setWeight but relying on that
		// is a fun regression waiting to happen.
		activeConfigStr, err := json.Marshal(activeConfig)
		if err != nil {
			return fmt.Errorf("unmarshaling exp config %v: %w", activeConfig, err)
		}
		if _, err := tx.NewUpdate().Model(&model.Experiment{}).
			Set("config = ?", string(activeConfigStr)).
			Where("id = ?", req.Id).
			Exec(ctx); err != nil {
			return fmt.Errorf("updating experiments config: %w", err)
		}

		// Zero out trial restarts. We do somewhat lose information about how many times
		// the previous failed but likely people care only about current run.
		var trialIDs []int32
		for _, t := range trialsResp.Trials {
			trialIDs = append(trialIDs, t.Id)
		}
		if len(trialIDs) > 0 {
			if _, err := tx.NewUpdate().Table("runs"). // TODO(nick-runs) call runs package.
									Set("restarts = 0").
									Set("end_time = null").
									Where("id IN (?)", bun.In(trialIDs)).
									Exec(ctx); err != nil {
				return fmt.Errorf("zeroing out trial restarts: %w", err)
			}
		}

		e.continueTrials = true

		// Check at the end to minimize chance of experiment already being created somehow.
		if _, ok := experiment.ExperimentRegistry.Load(int(req.Id)); ok {
			return fmt.Errorf("experiment already exists")
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("experiment continue database updates: %w", err)
	}

	if err = e.Start(); err != nil {
		return nil, errors.Wrapf(err, "failed to start experiment %d", e.ID)
	}
	started = true

	_, err = a.ActivateExperiment(ctx, &apiv1.ActivateExperimentRequest{Id: int32(e.ID)})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to activate experiment: %s", err)
	}

	protoExp, err := a.getExperiment(ctx, *actor, int(req.Id))
	if err != nil {
		return nil, err
	}
	return &apiv1.ContinueExperimentResponse{
		Experiment: protoExp,
		Warnings:   command.LaunchWarningToProto(launchWarnings),
	}, nil
}
