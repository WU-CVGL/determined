package user

import (
	"context"
	"crypto/sha512"
	"database/sql"
	"errors"
	"fmt"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
)

const (
	determinedUsername = "determined"
	adminUsername      = "admin"
)

// BuiltInUsers are created in the DB by the initial migration. They exist on every installation unless the
// admin has removed them.
var BuiltInUsers = []string{determinedUsername, adminUsername}

const clientSidePasswordSalt = "GubPEmmotfiK9TMD6Zdw" // #nosec G101

// ReplicateClientSideSaltAndHash replicates the password salt and hash done on the client side.
// We need this because we hash passwords on the client side, but when SCIM posts a user with
// a password to password sync, it doesn't - so when we try to log in later, we get a weird,
// unrecognizable sha512 hash from the frontend.
func ReplicateClientSideSaltAndHash(password string) string {
	if password == "" {
		return password
	}
	sum := sha512.Sum512([]byte(clientSidePasswordSalt + password))
	return fmt.Sprintf("%x", sum) // nolint: perfsprint
}

// SetUserPassword sets the password of the user with the given username to the plaintext string provided.
func SetUserPassword(ctx context.Context, username, password string) error {
	u, err := ByUsername(ctx, username)
	if err != nil {
		return fmt.Errorf("retrieving user %s: %w", username, err)
	}

	err = u.UpdatePasswordHash(ReplicateClientSideSaltAndHash(password))
	if err != nil {
		return fmt.Errorf("updating password hash for user %s: %w", username, err)
	}

	err = Update(ctx, u, []string{"password_hash"}, nil)
	if err != nil {
		return fmt.Errorf("updating password hash for user %s: %w", username, err)
	}
	return nil
}

var (
	// ErrCurrentPasswordRequired means that users tried to change their own password without
	// sending their current password.
	ErrCurrentPasswordRequired = errors.New(
		"enter your current password to change your own password")
	// ErrCurrentPasswordIncorrect means that users tried to change their own password with a
	// current password that does not match.
	ErrCurrentPasswordIncorrect = errors.New("the current password is incorrect")
	// ErrRemoteUserPassword means that a user who cannot sign in with a password tried to set their
	// own password. Remote users sign in through an external identity provider; an administrator can
	// still set a password for them.
	ErrRemoteUserPassword = errors.New(
		"remote users sign in through single sign-on and cannot set their own password")
)

// CheckCurrentPassword makes users who change their own password prove that they know the current
// one, so that a stolen token or a session ridden by another page cannot lock them out of their
// account. It returns nil when curUser changes another user's password; whether that is allowed is
// up to the authorization checks. currentPassword is nil when the client sent none. It is pre-salted
// and hashed when isHashed is set, like the new password. Users who have a blank password pass an
// empty string. Users who cannot sign in with a password at all (remote users, and users created
// with a password that disables password sign-in) cannot prove one, so they cannot set their own;
// otherwise a session obtained through single sign-on could add a password sign-in to the account.
// The comparison costs a bcrypt hash, which also slows down guessing through this check.
func CheckCurrentPassword(
	ctx context.Context, curUser model.User, targetID model.UserID, currentPassword *string,
	isHashed bool,
) error {
	if curUser.ID != targetID {
		return nil
	}
	// curUser may come from a cache or a token, so read the stored hash.
	var target model.User
	err := db.Bun().NewSelect().Model(&target).Where("id = ?", targetID).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return db.ErrNotFound
	case err != nil:
		return fmt.Errorf("looking up user %d: %w", targetID, err)
	}
	if target.Remote || target.PasswordHash == model.NoPasswordLogin {
		return ErrRemoteUserPassword
	}
	if currentPassword == nil {
		return ErrCurrentPasswordRequired
	}
	hashed := *currentPassword
	if !isHashed {
		hashed = ReplicateClientSideSaltAndHash(hashed)
	}
	if !target.ValidatePassword(hashed) {
		return ErrCurrentPasswordIncorrect
	}
	return nil
}
