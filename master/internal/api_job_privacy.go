package internal

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// errJobPrivacyUnimplemented is the answer of the job privacy RPCs until they are implemented.
var errJobPrivacyUnimplemented = status.Error(codes.Unimplemented, "job privacy is not implemented")

// GetJobPrivacyPolicy returns the cluster's job privacy policy.
func (a *apiServer) GetJobPrivacyPolicy(
	ctx context.Context, req *apiv1.GetJobPrivacyPolicyRequest,
) (*apiv1.GetJobPrivacyPolicyResponse, error) {
	return nil, errJobPrivacyUnimplemented
}

// PutJobPrivacyPolicy sets the cluster's job privacy policy.
func (a *apiServer) PutJobPrivacyPolicy(
	ctx context.Context, req *apiv1.PutJobPrivacyPolicyRequest,
) (*apiv1.PutJobPrivacyPolicyResponse, error) {
	return nil, errJobPrivacyUnimplemented
}

// GetMyJobPrivacy returns the requesting user's job privacy preferences.
func (a *apiServer) GetMyJobPrivacy(
	ctx context.Context, req *apiv1.GetMyJobPrivacyRequest,
) (*apiv1.GetMyJobPrivacyResponse, error) {
	return nil, errJobPrivacyUnimplemented
}

// PatchMyJobPrivacy changes the requesting user's job privacy preferences.
func (a *apiServer) PatchMyJobPrivacy(
	ctx context.Context, req *apiv1.PatchMyJobPrivacyRequest,
) (*apiv1.PatchMyJobPrivacyResponse, error) {
	return nil, errJobPrivacyUnimplemented
}

// GetJobSharing returns a job's own sharing.
func (a *apiServer) GetJobSharing(
	ctx context.Context, req *apiv1.GetJobSharingRequest,
) (*apiv1.GetJobSharingResponse, error) {
	return nil, errJobPrivacyUnimplemented
}

// PatchJobSharing changes a job's own sharing.
func (a *apiServer) PatchJobSharing(
	ctx context.Context, req *apiv1.PatchJobSharingRequest,
) (*apiv1.PatchJobSharingResponse, error) {
	return nil, errJobPrivacyUnimplemented
}
