package command

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/pkg/model"
)

func TestCanControlGenericTaskBasic(t *testing.T) {
	ownerID := model.UserID(1)
	tests := []struct {
		name      string
		user      model.User
		ownerID   *model.UserID
		wantError bool
	}{
		{
			name:    "owner",
			user:    model.User{ID: ownerID},
			ownerID: &ownerID,
		},
		{
			name:    "admin",
			user:    model.User{ID: 2, Admin: true},
			ownerID: &ownerID,
		},
		{
			name:      "other user",
			user:      model.User{ID: 2},
			ownerID:   &ownerID,
			wantError: true,
		},
		{
			name:      "missing owner",
			user:      model.User{ID: ownerID},
			wantError: true,
		},
		{
			name: "admin with missing owner",
			user: model.User{ID: 2, Admin: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (&NSCAuthZBasic{}).CanControlGenericTask(
				context.Background(), test.user, 1, test.ownerID,
			)
			if test.wantError {
				require.True(t, authz.IsPermissionDenied(err))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCanGetTensorBoardSourcesBasic(t *testing.T) {
	ctx := context.Background()
	user := model.User{ID: 1}
	for _, impl := range []NSCAuthZ{&NSCAuthZBasic{}, &NSCAuthZPermissive{}} {
		require.True(t, authz.IsPermissionDenied(impl.CanGetTensorBoardSources(ctx, user, nil, nil)))
		require.True(t, authz.IsPermissionDenied(
			impl.CanGetTensorBoardSources(ctx, user, []int32{}, []int32{})))
	}

	basic := &NSCAuthZBasic{}
	require.NoError(t, basic.CanGetTensorBoardSources(ctx, user, []int32{1}, nil))
	require.NoError(t, basic.CanGetTensorBoardSources(ctx, user, nil, []int32{2}))
	require.NoError(t, basic.CanGetTensorBoardSources(ctx, user, []int32{1}, []int32{2}))
}
