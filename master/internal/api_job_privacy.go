package internal

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

// jobPrivacyViewer returns the requesting user and the user as a job privacy viewer.
func jobPrivacyViewer(ctx context.Context) (*model.User, jobprivacy.Viewer, error) {
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, jobprivacy.Viewer{}, err
	}
	v, err := jobprivacy.NewViewer(ctx, *curUser)
	if err != nil {
		return nil, jobprivacy.Viewer{}, err
	}
	return curUser, v, nil
}

// audienceChoice is the API choice of an audience: nil when it is unset, else whether it is all
// users.
func audienceChoice(a *jobv1.JobAudience) *bool {
	if a == nil {
		return nil
	}
	allUsers := a.AllUsers
	return &allUsers
}

// GetJobPrivacyPolicy returns the cluster's job privacy policy to any user, so that clients can
// disable the choices it forbids.
func (a *apiServer) GetJobPrivacyPolicy(
	ctx context.Context, req *apiv1.GetJobPrivacyPolicyRequest,
) (*apiv1.GetJobPrivacyPolicyResponse, error) {
	if _, _, err := grpcutil.GetUser(ctx); err != nil {
		return nil, err
	}
	p, err := jobprivacy.ReadPolicy(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &apiv1.GetJobPrivacyPolicyResponse{Rules: p.Rules()}, nil
}

// PutJobPrivacyPolicy sets the cluster's job privacy policy. Only admins may, and the request has
// exactly one rule per content kind.
func (a *apiServer) PutJobPrivacyPolicy(
	ctx context.Context, req *apiv1.PutJobPrivacyPolicyRequest,
) (*apiv1.PutJobPrivacyPolicyResponse, error) {
	curUser, v, err := jobPrivacyViewer(ctx)
	if err != nil {
		return nil, err
	}
	if !v.Admin {
		return nil, status.Error(codes.PermissionDenied,
			"only admins set the job privacy policy")
	}
	rules := make(map[jobprivacy.Kind]jobprivacy.KindPolicy, len(req.Rules))
	for _, r := range req.Rules {
		k, ok := jobprivacy.KindFromProto(r.Kind)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "invalid job content kind %s", r.Kind)
		}
		if _, dup := rules[k]; dup {
			return nil, status.Errorf(codes.InvalidArgument, "more than one rule for %s", r.Kind)
		}
		if r.NewJobAudience == nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"the rule for %s has no new_job_audience", r.Kind)
		}
		rules[k] = jobprivacy.KindPolicy{
			Allowed:        r.SharingAllowed,
			NewJobAudience: jobprivacy.Audience(r.NewJobAudience.AllUsers),
		}
	}
	for _, k := range jobprivacy.Kinds {
		if _, ok := rules[k]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "no rule for %s", k.Proto())
		}
	}
	p, err := jobprivacy.WritePolicy(ctx, curUser.ID, rules)
	if err != nil {
		return nil, err
	}
	return &apiv1.PutJobPrivacyPolicyResponse{Rules: p.Rules()}, nil
}

// GetMyJobPrivacy returns the requesting user's job privacy preferences.
func (a *apiServer) GetMyJobPrivacy(
	ctx context.Context, req *apiv1.GetMyJobPrivacyRequest,
) (*apiv1.GetMyJobPrivacyResponse, error) {
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}
	u, err := jobprivacy.ReadUserPrivacy(ctx, nil, curUser.ID)
	if err != nil {
		return nil, err
	}
	return &apiv1.GetMyJobPrivacyResponse{Privacy: u.Proto()}, nil
}

// PatchMyJobPrivacy changes the requesting user's job privacy preferences.
func (a *apiServer) PatchMyJobPrivacy(
	ctx context.Context, req *apiv1.PatchMyJobPrivacyRequest,
) (*apiv1.PatchMyJobPrivacyResponse, error) {
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}
	u, err := jobprivacy.PatchUserPrivacy(ctx, curUser.ID, jobprivacy.UserPrivacyPatch{
		ShareTitles:       req.ShareTitles,
		ShareLogs:         req.ShareLogs,
		NewJobTitles:      audienceChoice(req.NewJobTitles),
		NewJobLogs:        audienceChoice(req.NewJobLogs),
		ResetNewJobTitles: req.ResetNewJobTitles,
		ResetNewJobLogs:   req.ResetNewJobLogs,
	})
	if err != nil {
		return nil, err
	}
	return &apiv1.PatchMyJobPrivacyResponse{Privacy: u.Proto()}, nil
}

// GetJobSharing returns a job's own sharing and the cluster policy to the job's owner or an
// admin.
func (a *apiServer) GetJobSharing(
	ctx context.Context, req *apiv1.GetJobSharingRequest,
) (*apiv1.GetJobSharingResponse, error) {
	_, v, err := jobPrivacyViewer(ctx)
	if err != nil {
		return nil, err
	}
	s, err := jobprivacy.GetSharing(ctx, v, model.JobID(req.JobId))
	if err != nil {
		return nil, err
	}
	p, err := jobprivacy.ReadPolicy(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &apiv1.GetJobSharingResponse{Sharing: s.Proto(), Rules: p.Rules()}, nil
}

// PatchJobSharing changes a job's own sharing for the job's owner or an admin.
func (a *apiServer) PatchJobSharing(
	ctx context.Context, req *apiv1.PatchJobSharingRequest,
) (*apiv1.PatchJobSharingResponse, error) {
	_, v, err := jobPrivacyViewer(ctx)
	if err != nil {
		return nil, err
	}
	s, err := jobprivacy.PatchSharing(ctx, v, model.JobID(req.JobId), jobprivacy.SharingPatch{
		Title:  audienceChoice(req.Title),
		Logs:   audienceChoice(req.Logs),
		Config: audienceChoice(req.Config),
	})
	if err != nil {
		return nil, err
	}
	return &apiv1.PatchJobSharingResponse{Sharing: s.Proto()}, nil
}
