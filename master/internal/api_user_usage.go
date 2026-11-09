package internal

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// GetUserResources returns the resources a user's tasks hold now. It is not implemented yet.
func (a *apiServer) GetUserResources(
	ctx context.Context, req *apiv1.GetUserResourcesRequest,
) (*apiv1.GetUserResourcesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GetUserResources is not implemented")
}

// GetUserUsage returns a user's daily or monthly resource usage. It is not implemented yet.
func (a *apiServer) GetUserUsage(
	ctx context.Context, req *apiv1.GetUserUsageRequest,
) (*apiv1.GetUserUsageResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GetUserUsage is not implemented")
}
