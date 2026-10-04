//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/userv1"
)

// addPasswordUser creates an active, non-admin user with the given plaintext password.
func addPasswordUser(t *testing.T, password string) model.User {
	u := model.User{Username: uuid.New().String(), Active: true}
	require.NoError(t, u.UpdatePasswordHash(user.ReplicateClientSideSaltAndHash(password)))
	id, err := user.Add(context.Background(), &u, nil)
	require.NoError(t, err)
	u.ID = id
	return u
}

// sessionContext logs u in and returns a context that carries the new session token.
func sessionContext(t *testing.T, u model.User) context.Context {
	token, err := user.StartSession(context.Background(), &u)
	require.NoError(t, err)
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-user-token", fmt.Sprintf("Bearer %s", token)))
}

func requireCode(t *testing.T, code codes.Code, err error) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, status.Code(err), err.Error())
}

func requireLogin(t *testing.T, api *apiServer, username, password string, ok bool) {
	t.Helper()
	_, err := api.Login(context.Background(), &apiv1.LoginRequest{
		Username: username, Password: password,
	})
	if ok {
		require.NoError(t, err)
	} else {
		require.Error(t, err)
	}
}

func TestSetUserPasswordRequiresCurrentPasswordForSelf(t *testing.T) {
	api, admin, adminCtx := setupAPITest(t, nil)
	const oldPassword = "Old-password-1"
	u := addPasswordUser(t, oldPassword)
	req := func(old *string, newPassword string) *apiv1.SetUserPasswordRequest {
		return &apiv1.SetUserPasswordRequest{
			UserId: int32(u.ID), Password: newPassword, OldPassword: old,
		}
	}

	ctx := sessionContext(t, u)
	_, err := api.SetUserPassword(ctx, req(nil, "New-password-1"))
	requireCode(t, codes.InvalidArgument, err)
	_, err = api.SetUserPassword(ctx, req(ptrs.Ptr("wrong"), "New-password-1"))
	requireCode(t, codes.PermissionDenied, err)
	// A blank current password is a wrong password for a user who has one.
	_, err = api.SetUserPassword(ctx, req(ptrs.Ptr(""), "New-password-1"))
	requireCode(t, codes.PermissionDenied, err)
	requireLogin(t, api, u.Username, oldPassword, true)

	_, err = api.SetUserPassword(ctx, req(ptrs.Ptr(oldPassword), "New-password-1"))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, oldPassword, false)
	requireLogin(t, api, u.Username, "New-password-1", true)

	// An administrator needs no current password to change another user's password.
	_, err = api.SetUserPassword(adminCtx, req(nil, "New-password-2"))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, "New-password-2", true)

	// Administrators changing their own password need it like everyone else. The test
	// administrator has no password, so an empty string is its current password.
	adminReq := &apiv1.SetUserPasswordRequest{UserId: int32(admin.ID), Password: "Admin-password-1"}
	_, err = api.SetUserPassword(adminCtx, adminReq)
	requireCode(t, codes.InvalidArgument, err)
	adminReq.OldPassword = ptrs.Ptr("not-blank")
	_, err = api.SetUserPassword(adminCtx, adminReq)
	requireCode(t, codes.PermissionDenied, err)
	adminReq.OldPassword = ptrs.Ptr("")
	_, err = api.SetUserPassword(adminCtx, adminReq)
	require.NoError(t, err)
	requireLogin(t, api, admin.Username, "Admin-password-1", true)
}

func TestPatchUserRequiresCurrentPasswordForSelf(t *testing.T) {
	api, _, adminCtx := setupAPITest(t, nil)
	const oldPassword = "Old-password-1"
	u := addPasswordUser(t, oldPassword)
	patch := func(newPassword string, old *string, hashed bool) *apiv1.PatchUserRequest {
		if hashed {
			newPassword = user.ReplicateClientSideSaltAndHash(newPassword)
		}
		return &apiv1.PatchUserRequest{UserId: int32(u.ID), User: &userv1.PatchUser{
			Password: ptrs.Ptr(newPassword), OldPassword: old, IsHashed: hashed,
		}}
	}
	hash := user.ReplicateClientSideSaltAndHash

	ctx := sessionContext(t, u)
	_, err := api.PatchUser(ctx, patch("New-password-1", nil, true))
	requireCode(t, codes.InvalidArgument, err)
	_, err = api.PatchUser(ctx, patch("New-password-1", ptrs.Ptr(hash("wrong")), true))
	requireCode(t, codes.PermissionDenied, err)
	// The current password is hashed like the new one: a plaintext one with is_hashed is wrong.
	_, err = api.PatchUser(ctx, patch("New-password-1", ptrs.Ptr(oldPassword), true))
	requireCode(t, codes.PermissionDenied, err)
	requireLogin(t, api, u.Username, oldPassword, true)

	_, err = api.PatchUser(ctx, patch("New-password-1", ptrs.Ptr(hash(oldPassword)), true))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, "New-password-1", true)

	// Changing the password ends the user's sessions, so sign in again.
	ctx = sessionContext(t, u)
	_, err = api.PatchUser(ctx, patch("New-password-2", ptrs.Ptr("New-password-1"), false))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, "New-password-2", true)

	// Other fields need no current password.
	ctx = sessionContext(t, u)
	_, err = api.PatchUser(ctx, &apiv1.PatchUserRequest{UserId: int32(u.ID), User: &userv1.PatchUser{
		DisplayName: ptrs.Ptr(uuid.New().String()),
	}})
	require.NoError(t, err)

	// An administrator needs no current password to change another user's password.
	_, err = api.PatchUser(adminCtx, patch("New-password-3", nil, false))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, "New-password-3", true)
}

func TestChangeOwnPasswordBlankAndRemoteUsers(t *testing.T) {
	api, _, _ := setupAPITest(t, nil)

	// A user with a blank password sends an empty current password.
	blank := model.User{Username: uuid.New().String(), Active: true}
	id, err := user.Add(context.Background(), &blank, nil)
	require.NoError(t, err)
	blank.ID = id
	ctx := sessionContext(t, blank)
	_, err = api.PatchUser(ctx, &apiv1.PatchUserRequest{UserId: int32(id), User: &userv1.PatchUser{
		Password: ptrs.Ptr(user.ReplicateClientSideSaltAndHash("New-password-1")), IsHashed: true,
	}})
	requireCode(t, codes.InvalidArgument, err)
	_, err = api.PatchUser(ctx, &apiv1.PatchUserRequest{UserId: int32(id), User: &userv1.PatchUser{
		Password:    ptrs.Ptr(user.ReplicateClientSideSaltAndHash("New-password-1")),
		OldPassword: ptrs.Ptr(""),
		IsHashed:    true,
	}})
	require.NoError(t, err)
	requireLogin(t, api, blank.Username, "New-password-1", true)

	// Remote users sign in through SSO and cannot set a password for themselves, and neither can
	// users whose password disables password sign-in.
	for _, remoteFlag := range []bool{true, false} {
		remote := model.User{
			Username: uuid.New().String(), Active: true, Remote: remoteFlag,
			PasswordHash: model.NoPasswordLogin,
		}
		id, err = user.Add(context.Background(), &remote, nil)
		require.NoError(t, err)
		remote.ID = id
		ctx = sessionContext(t, remote)
		for _, old := range []*string{nil, ptrs.Ptr("")} {
			_, err = api.SetUserPassword(ctx, &apiv1.SetUserPasswordRequest{
				UserId: int32(id), Password: "New-password-1", OldPassword: old,
			})
			requireCode(t, codes.InvalidArgument, err)
			require.Contains(t, err.Error(), "remote users")
		}
	}
}
