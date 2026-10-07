package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ghodss/yaml"
	structpb "github.com/golang/protobuf/ptypes/struct"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/configpolicy"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/license"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

const (
	noWorkloadErr          = "no workload type specified"
	noPoliciesErr          = "no specified config policies"
	globalPriorityErr      = "global priority limit already exists"
	invalidWorkloadTypeErr = "invalid workload type"
	settingPoliciesErr     = "error setting task config policies"
)

func (a *apiServer) validatePoliciesAndWorkloadType(
	globalConfigPolicies *model.TaskConfigPolicies,
	workloadType string,
	configPolicies string,
) error {
	// Validate workload type
	if !configpolicy.ValidWorkloadType(workloadType) {
		errMessage := fmt.Errorf(invalidWorkloadTypeErr+": %s", workloadType)
		if len(workloadType) == 0 {
			errMessage = fmt.Errorf(noWorkloadErr)
		}
		return errMessage
	}

	// Validate policies.
	if len(configPolicies) == 0 {
		return fmt.Errorf(noPoliciesErr)
	}

	// Validate the input config based on workload type.
	_, priorityEnabledErr := a.m.rm.SmallerValueIsHigherPriority()
	switch workloadType {
	case model.ExperimentType:
		return configpolicy.ValidateExperimentConfig(globalConfigPolicies, configPolicies,
			priorityEnabledErr)
	case model.NTSCType:
		return configpolicy.ValidateNTSCConfig(globalConfigPolicies, configPolicies,
			priorityEnabledErr)
	default:
		return fmt.Errorf(invalidWorkloadTypeErr+": %s", workloadType)
	}
}

func parseConfigPolicies(configAndConstraints string) (
	tcps map[string]interface{}, invariantConfig *string, constraints *string, err error,
) {
	if len(configAndConstraints) == 0 {
		return nil, nil, nil, fmt.Errorf("nothing to parse, empty " +
			"config and constraints input")
	}
	// Standardize to JSON policies file format.
	configPolicies, err := yaml.YAMLToJSON([]byte(configAndConstraints))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error parsing config policies: %w", err)
	}
	// Extract individual config and constraints.
	var policies map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(configPolicies))
	err = dec.Decode(&policies)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error unmarshaling config policies: %s", err.Error())
	}
	var configPolicy *string
	if invariantConfig, ok := policies["invariant_config"]; ok {
		configPolicyBytes, err := json.Marshal(invariantConfig)
		if err != nil {
			return nil, nil, nil,
				fmt.Errorf("error marshaling input invariant config policy: %s", err.Error())
		}
		configPolicy = ptrs.Ptr(string(configPolicyBytes))
	}

	var constraintsPolicy *string
	if constraints, ok := policies["constraints"]; ok {
		constraintsPolicyBytes, err := json.Marshal(constraints)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("error marshaling input constraints policy: %s",
				err.Error())
		}
		constraintsPolicy = ptrs.Ptr(string(constraintsPolicyBytes))
	}

	return policies, configPolicy, constraintsPolicy, nil
}

// invariantExperimentPool returns the resource pool that the invariant config of an experiment
// config policy pins, or "" when it pins none. invariantConfig is that invariant config as JSON.
func invariantExperimentPool(invariantConfig *string) (string, error) {
	if invariantConfig == nil {
		return "", nil
	}
	var config expconf.ExperimentConfigV0
	if err := json.Unmarshal([]byte(*invariantConfig), &config); err != nil {
		return "", err
	}
	if config.RawResources == nil || config.RawResources.RawResourcePool == nil {
		return "", nil
	}
	return *config.RawResources.RawResourcePool, nil
}

// canSetConfigPolicyPool checks that user may save the config policy of workloadType whose
// invariant config is invariantConfig (JSON) for workspaceID, or globally when workspaceID is nil.
// An experiment policy's pool replaces the pool of every experiment in its scope, also one that
// names its own, so it follows the rule of a workspace default pool: see canSetDefaultPool. Only
// experiment policies can pin a pool; NTSC policies have no invariant config.
func canSetConfigPolicyPool(
	ctx context.Context, user model.User, workspaceID *int, workloadType string,
	invariantConfig *string,
) error {
	if workloadType != model.ExperimentType {
		return nil
	}
	pool, err := invariantExperimentPool(invariantConfig)
	if err != nil {
		return status.Errorf(codes.InvalidArgument,
			configpolicy.InvalidExperimentConfigPolicyErr+": %s", err)
	}
	if pool == "" {
		return nil
	}
	current, err := configpolicy.GetTaskConfigPolicies(ctx, workspaceID, workloadType)
	if err != nil {
		return status.Errorf(codes.Internal, "reading the current config policy: %s", err)
	}
	currentPool, err := invariantExperimentPool(current.InvariantConfig)
	if err != nil {
		return status.Errorf(codes.Internal, "reading the current config policy: %s", err)
	}
	return canSetDefaultPool(ctx, user, pool, currentPool)
}

// Add or update workspace task config policies.
func (a *apiServer) PutWorkspaceConfigPolicies(
	ctx context.Context, req *apiv1.PutWorkspaceConfigPoliciesRequest,
) (*apiv1.PutWorkspaceConfigPoliciesResponse, error) {
	license.RequireLicense("manage config policies")

	// Request Validation
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	w, err := a.GetWorkspaceByID(ctx, req.WorkspaceId, *curUser, false)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	err = configpolicy.AuthZProvider.Get().CanModifyWorkspaceConfigPolicies(ctx, *curUser, w)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, err.Error())
	}

	globalConfigPolicies, err := configpolicy.GetTaskConfigPolicies(ctx, nil, req.WorkloadType)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	err = a.validatePoliciesAndWorkloadType(globalConfigPolicies, req.WorkloadType,
		req.ConfigPolicies)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	configPolicies, invariantConfig, constraints, err := parseConfigPolicies(req.ConfigPolicies)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	// Returned as is: a refusal is PermissionDenied, and access that could not be read Unavailable.
	if err = canSetConfigPolicyPool(ctx, *curUser, ptrs.Ptr(int(req.WorkspaceId)),
		req.WorkloadType, invariantConfig); err != nil {
		return nil, err
	}

	err = configpolicy.SetTaskConfigPolicies(ctx, &model.TaskConfigPolicies{
		WorkspaceID:     ptrs.Ptr(int(req.WorkspaceId)),
		WorkloadType:    req.WorkloadType,
		LastUpdatedBy:   curUser.ID,
		LastUpdatedTime: time.Now(),
		InvariantConfig: invariantConfig,
		Constraints:     constraints,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, settingPoliciesErr+": "+err.Error())
	}

	return &apiv1.PutWorkspaceConfigPoliciesResponse{
			ConfigPolicies: configpolicy.MarshalConfigPolicy(configPolicies),
		},
		nil
}

// Add or update global task config policies.
func (a *apiServer) PutGlobalConfigPolicies(
	ctx context.Context, req *apiv1.PutGlobalConfigPoliciesRequest,
) (*apiv1.PutGlobalConfigPoliciesResponse, error) {
	license.RequireLicense("manage config policies")

	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, err.Error())
	}

	err = configpolicy.AuthZProvider.Get().CanModifyGlobalConfigPolicies(ctx, curUser)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, err.Error())
	}

	err = a.validatePoliciesAndWorkloadType(nil, req.WorkloadType, req.ConfigPolicies)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	configPolicies, invariantConfig, constraints, err := parseConfigPolicies(req.ConfigPolicies)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	err = canSetConfigPolicyPool(ctx, *curUser, nil, req.WorkloadType, invariantConfig)
	if err != nil {
		return nil, err
	}

	err = configpolicy.SetTaskConfigPolicies(ctx, &model.TaskConfigPolicies{
		WorkloadType:    req.WorkloadType,
		LastUpdatedBy:   curUser.ID,
		LastUpdatedTime: time.Now(),
		InvariantConfig: invariantConfig,
		Constraints:     constraints,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, settingPoliciesErr+": "+err.Error())
	}

	return &apiv1.PutGlobalConfigPoliciesResponse{
			ConfigPolicies: configpolicy.MarshalConfigPolicy(configPolicies),
		},
		nil
}

// Get workspace task config policies.
func (a *apiServer) GetWorkspaceConfigPolicies(
	ctx context.Context, req *apiv1.GetWorkspaceConfigPoliciesRequest,
) (*apiv1.GetWorkspaceConfigPoliciesResponse, error) {
	license.RequireLicense("manage config policies")

	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	w, err := a.GetWorkspaceByID(ctx, req.WorkspaceId, *curUser, false)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	err = configpolicy.AuthZProvider.Get().CanViewWorkspaceConfigPolicies(ctx, *curUser, w)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, err.Error())
	}

	resp, err := a.getConfigPolicies(ctx, ptrs.Ptr(int(req.WorkspaceId)), req.WorkloadType)
	if err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	return &apiv1.GetWorkspaceConfigPoliciesResponse{ConfigPolicies: resp}, nil
}

// Get global task config policies.
func (a *apiServer) GetGlobalConfigPolicies(
	ctx context.Context, req *apiv1.GetGlobalConfigPoliciesRequest,
) (*apiv1.GetGlobalConfigPoliciesResponse, error) {
	license.RequireLicense("manage config policies")

	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	err = configpolicy.AuthZProvider.Get().CanViewGlobalConfigPolicies(ctx, curUser)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, err.Error())
	}

	resp, err := a.getConfigPolicies(ctx, nil, req.WorkloadType)
	if err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	return &apiv1.GetGlobalConfigPoliciesResponse{ConfigPolicies: resp}, nil
}

func (*apiServer) getConfigPolicies(
	ctx context.Context, workspaceID *int, workloadType string,
) (*structpb.Struct, error) {
	if !configpolicy.ValidWorkloadType(workloadType) {
		errMessage := fmt.Sprintf(invalidWorkloadTypeErr+": %s.", workloadType)
		if len(workloadType) == 0 {
			errMessage = noWorkloadErr
		}
		return nil, status.Errorf(codes.InvalidArgument, errMessage)
	}

	configPolicies, err := configpolicy.GetTaskConfigPolicies(
		ctx, workspaceID, workloadType)
	if err != nil {
		return nil, err
	}
	policyMap := map[string]interface{}{}
	if configPolicies.InvariantConfig != nil {
		var configMap map[string]interface{}
		if err := yaml.Unmarshal([]byte(*configPolicies.InvariantConfig), &configMap); err != nil {
			return nil, fmt.Errorf("unable to unmarshal json: %w", err)
		}
		policyMap["invariant_config"] = configMap
	}
	if configPolicies.Constraints != nil {
		var constraintsMap map[string]interface{}
		if err := yaml.Unmarshal([]byte(*configPolicies.Constraints), &constraintsMap); err != nil {
			return nil, fmt.Errorf("unable to unmarshal json: %w", err)
		}
		policyMap["constraints"] = constraintsMap
	}
	return configpolicy.MarshalConfigPolicy(policyMap), nil
}

// Delete workspace task config policies.
func (a *apiServer) DeleteWorkspaceConfigPolicies(
	ctx context.Context, req *apiv1.DeleteWorkspaceConfigPoliciesRequest,
) (*apiv1.DeleteWorkspaceConfigPoliciesResponse, error) {
	license.RequireLicense("manage config policies")

	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	w, err := a.GetWorkspaceByID(ctx, req.WorkspaceId, *curUser, false)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	err = configpolicy.AuthZProvider.Get().CanModifyWorkspaceConfigPolicies(ctx, *curUser, w)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}

	if !configpolicy.ValidWorkloadType(req.WorkloadType) {
		errMessage := fmt.Sprintf(invalidWorkloadTypeErr+": %s.", req.WorkloadType)
		if len(req.WorkloadType) == 0 {
			errMessage = noWorkloadErr
		}
		return nil, status.Errorf(codes.InvalidArgument, errMessage)
	}

	err = configpolicy.DeleteConfigPolicies(ctx, ptrs.Ptr(int(req.WorkspaceId)),
		req.WorkloadType)
	if err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}
	return &apiv1.DeleteWorkspaceConfigPoliciesResponse{}, nil
}

// Delete global task config policies.
func (a *apiServer) DeleteGlobalConfigPolicies(
	ctx context.Context, req *apiv1.DeleteGlobalConfigPoliciesRequest,
) (*apiv1.DeleteGlobalConfigPoliciesResponse, error) {
	license.RequireLicense("manage config policies")

	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	err = configpolicy.AuthZProvider.Get().CanModifyGlobalConfigPolicies(ctx, curUser)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, err.Error())
	}

	if !configpolicy.ValidWorkloadType(req.WorkloadType) {
		errMessage := fmt.Sprintf(invalidWorkloadTypeErr+": %s.", req.WorkloadType)
		if len(req.WorkloadType) == 0 {
			errMessage = noWorkloadErr
		}
		return nil, status.Errorf(codes.InvalidArgument, errMessage)
	}

	err = configpolicy.DeleteConfigPolicies(ctx, nil, req.WorkloadType)
	if err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}
	return &apiv1.DeleteGlobalConfigPoliciesResponse{}, nil
}
