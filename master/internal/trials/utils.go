package trials

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/pkg/model"
)

// CanGetTrialsExperimentAndCheckCanDoAction is a utility function for generalizing
// RBAC support for trials and experiments.
func CanGetTrialsExperimentAndCheckCanDoAction(ctx context.Context,
	trialID int, curUser *model.User, actionFunc func(context.Context, model.User, *model.Experiment) error,
) error {
	trialNotFound := api.NotFoundErrs("trial", strconv.Itoa(trialID), true)
	exp, err := db.ExperimentByTrialID(ctx, trialID)
	if errors.Is(err, db.ErrNotFound) {
		return trialNotFound
	} else if err != nil {
		return err
	}
	if err = experiment.AuthZProvider.Get().CanGetExperiment(ctx, *curUser, exp); err != nil {
		return authz.SubIfUnauthorized(err, trialNotFound)
	}

	if err = actionFunc(ctx, *curUser, exp); err != nil {
		// An action that answers with a definite gRPC code, such as a job privacy refusal or an
		// unavailable policy, keeps its code and message.
		if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
			return err
		}
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return nil
}

// CanGetTrialsExperimentAndCheckCanDoActionBulk functions the same as
// CanGetTrialsExperimentAndCheckCanDoAction but takes in multiple trial ids.
func CanGetTrialsExperimentAndCheckCanDoActionBulk(ctx context.Context,
	trialIDs []int, curUser *model.User, actionFunc func(context.Context, model.User, *model.Experiment) error,
) error {
	idString, err := json.Marshal(trialIDs)
	if err != nil {
		return err
	}
	trialNotFound := api.NotFoundErrs("trial", strings.Trim(string(idString), "[]"), true)
	exps, err := db.ExperimentsByTrialID(ctx, trialIDs)
	if errors.Is(err, db.ErrNotFound) {
		return trialNotFound
	} else if err != nil {
		return err
	}
	for _, exp := range exps {
		if err = experiment.AuthZProvider.Get().CanGetExperiment(ctx, *curUser, exp); err != nil {
			return authz.SubIfUnauthorized(err, trialNotFound)
		}

		if err = actionFunc(ctx, *curUser, exp); err != nil {
			return status.Error(codes.PermissionDenied, err.Error())
		}
	}
	return nil
}
