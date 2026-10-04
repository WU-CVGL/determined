package user

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/o1egl/paseto"

	log "github.com/sirupsen/logrus"

	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/telemetry"
	"github.com/determined-ai/determined/master/pkg/model"
)

var (
	once        sync.Once
	userService *Service
)

var externalSessionsError = echo.NewHTTPError(
	http.StatusForbidden,
	"not enabled with external sessions")

var forbiddenError = echo.NewHTTPError(
	http.StatusForbidden,
	"user not authorized")

const (
	// authNone indicates a request needs no authentication.
	authNone int = 0
	// authStandard indicates a request needs authentication.
	authStandard = 1
)

// unauthenticatedPointsList contains URIs and paths that are exempted from authentication.
var unauthenticatedPointsList = []string{
	"/",
	"/docs/.*",
	"/info",
	"/task-logs",
	"/agents",
	"/det",
	"/det/.*",
	"/login",
	"/api/v1/.*",
	"/proxy/:service/.*",
	"/agents\\?id=.*",
	"/saml/sso(/.*)?",
	"/saml/initiate(/.*)?",
	"/oidc/callback(/.*)?",
	"/oidc/sso(/.*)?",
	"/oauth2/authorize(/.*)?",
	"/oauth2/token(/.*)?",
	"/scim/v2/.*",
}

var unauthenticatedPointsPattern = regexp.MustCompile("^" +
	strings.Join(unauthenticatedPointsList, "$|^") + "$")

type agentUserGroup struct {
	UID   *int   `json:"uid,omitempty"`
	GID   *int   `json:"gid,omitempty"`
	User  string `json:"user"`
	Group string `json:"group"`
}

func (h *agentUserGroup) Validate() (*model.AgentUserGroup, error) {
	switch {
	case h.UID == nil:
		return nil, errors.New("uid must be set")
	case h.GID == nil:
		return nil, errors.New("gid must be set")
	case len(h.User) == 0:
		return nil, errors.New("user must be set")
	case len(h.Group) == 0:
		return nil, errors.New("group must be set")
	}

	return &model.AgentUserGroup{
		UID:   *h.UID,
		GID:   *h.GID,
		User:  h.User,
		Group: h.Group,
	}, nil
}

// Service describes a user manager.
type Service struct {
	db        *db.PgDB
	extConfig *model.ExternalSessions
}

// InitService creates the user service singleton.
func InitService(db *db.PgDB, extConfig *model.ExternalSessions) {
	once.Do(func() {
		userService = &Service{db, extConfig}
		if extConfig != nil && userService.extConfig.Enabled() {
			cert, err := config.GetMasterConfig().Security.TLS.ReadCertificate()
			if err != nil {
				log.WithError(err).Errorf("failed to read the master TLS certificate")
				return
			}
			userService.extConfig.StartInvalidationPoll(cert)
		}
	})
}

// GetService returns a reference to the user service singleton.
func GetService() *Service {
	if userService == nil {
		panic("Singleton UserService is not yet initialized.")
	}
	return userService
}

// The middleware looks for a token in three places (in this order):
// 1. The HTTP Authorization header.
// 2. The "det_jwt" cookie, when external sessions are enabled.
// 3. The session cookie, SessionCookieName.
func (s *Service) extractToken(r *http.Request) (string, error) {
	authRaw := r.Header.Get("Authorization")
	if authRaw != "" {
		// We attempt to parse out the token, which should be
		// transmitted as a Bearer authentication token.
		if !strings.HasPrefix(authRaw, "Bearer ") {
			return "", echo.ErrUnauthorized
		}
		return strings.TrimPrefix(authRaw, "Bearer "), nil
	} else if cookie, err := r.Cookie("det_jwt"); s.extConfig.Enabled() && err == nil {
		return cookie.Value, nil
	} else if cookie, err := r.Cookie(SessionCookieName); err == nil {
		return cookie.Value, nil
	}
	// If we found no token, then abort the request with an HTTP 401.
	return "", echo.NewHTTPError(http.StatusUnauthorized)
}

// UserAndSessionFromRequest gets the user and session corresponding to the given request.
func (s *Service) UserAndSessionFromRequest(
	r *http.Request,
) (*model.User, *model.UserSession, error) {
	token, err := s.extractToken(r)
	if err != nil {
		return nil, nil, err
	}
	return ByToken(context.TODO(), token, s.extConfig)
}

// UserAndNotebookSessionFromToken gets the user and notebook session for a given token.
func (s *Service) UserAndNotebookSessionFromToken(
	token string,
) (*model.User, *model.NotebookSession, error) {
	var notebookSession model.NotebookSession
	ctx := context.TODO()
	v2 := paseto.NewV2()
	if err := v2.Verify(token, db.GetTokenKeys().PublicKey, &notebookSession, nil); err != nil {
		return nil, nil, db.ErrNotFound
	}
	var user model.User

	// Check if the user session ID exists; some notebooks launched before the change to use
	// user IDs may still have the session ID on the token.
	if notebookSession.SessionID != nil {
		user, err := BySessionID(context.TODO(), *notebookSession.SessionID)
		if err != nil {
			return nil, nil, err
		}
		return user, &notebookSession, nil
	}

	err := db.Bun().NewSelect().
		Table("users").
		Where("id = ?", notebookSession.UserID).Scan(ctx, &user)
	if err != nil {
		return nil, nil, err
	}
	return &user, &notebookSession, nil
}

// getAuthLevel returns what level of authentication a request needs.
func (s *Service) getAuthLevel(c echo.Context) int {
	switch {
	case unauthenticatedPointsPattern.MatchString(c.Path()):
		return authNone
	case unauthenticatedPointsPattern.MatchString(c.Request().RequestURI):
		return authNone
	default:
		return authStandard
	}
}

// ProcessAuthentication is a middleware processing function that attempts
// to authenticate incoming HTTP requests.
func (s *Service) ProcessAuthentication(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if s.getAuthLevel(c) == authNone {
			return next(c)
		}

		user, session, err := s.UserAndSessionFromRequest(c.Request())
		switch err {
		case nil:
			if !user.Active {
				return echo.NewHTTPError(http.StatusForbidden, "user not active")
			}

			// Set data on the request context that might be useful to
			// event handlers.
			c.(*detContext.DetContext).SetUser(*user)
			c.(*detContext.DetContext).SetUserSession(*session)
			return next(c)
		case db.ErrNotFound, ErrAccessTokenRevoked, ErrRemoteUserTokenExpired:
			// The session has ended, or the access token was revoked (by a new password, for one).
			return echo.NewHTTPError(http.StatusUnauthorized)
		default:
			return err
		}
	}
}

func (s *Service) postLogout(c echo.Context) (interface{}, error) {
	// ClearSessionCookieOnLogout has removed the session cookie.

	// Delete the user session information from the database.
	sess := c.(*detContext.DetContext).MustGetUserSession()

	if err := DeleteSessionByID(context.TODO(), sess.ID); err != nil {
		return nil, err
	}

	return "", nil
}

func (s *Service) postLogin(c echo.Context) (interface{}, error) {
	if s.extConfig.JwtKey != "" {
		return nil, echo.NewHTTPError(http.StatusMisdirectedRequest,
			"authentication is configured to be external")
	}

	type (
		request struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		response struct {
			Token string `json:"token"`
		}
	)

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, err
	}

	var params request
	if err = json.Unmarshal(body, &params); err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest)
	}

	// Get the user from the database.
	user, err := ByUsername(context.TODO(), params.Username)
	switch err {
	case nil:
	case db.ErrNotFound:
		return nil, echo.NewHTTPError(http.StatusForbidden, "user not found")
	default:
		return nil, err
	}

	if user.Remote { // We can't return a more specific error for informational leak reasons.
		return nil, echo.NewHTTPError(http.StatusForbidden, "invalid credentials")
	}

	// The user must be active.
	if !user.Active {
		return nil, echo.NewHTTPError(http.StatusForbidden, "user not active")
	}

	var token string
	if !user.ValidatePassword(params.Password) {
		return nil, echo.NewHTTPError(http.StatusForbidden, "invalid credentials")
	}

	token, err = StartSession(context.TODO(), user)
	if err != nil {
		return nil, err
	}

	// The caller of this REST endpoint can request that the master set a cookie.
	// This is used by the WebUI for persistence of sessions.
	if c.QueryParam("cookie") == "true" {
		c.SetCookie(NewSessionCookie(token, SessionCookieSecure(c.Request())))
	}

	return response{
		Token: token,
	}, nil
}

// postSessionCookie stores the token from the request's Authorization header in the browser's
// session cookie. The web UI calls it when it receives a token in its URL (?jwt=), which it used to
// write into the cookie itself before the cookie became HttpOnly. The token was checked by
// ProcessAuthentication. Like signing in, and unlike other requests with an Authorization header,
// it must come from the master's own pages (CrossOriginProtection checks this too): when
// enable_cors lets other origins send such headers, a page elsewhere could otherwise plant its own
// session in the visitor's browser.
func (s *Service) postSessionCookie(c echo.Context) (interface{}, error) {
	authRaw := c.Request().Header.Get(echo.HeaderAuthorization)
	if !strings.HasPrefix(authRaw, "Bearer ") {
		return nil, echo.NewHTTPError(http.StatusBadRequest,
			"send the token to store in an Authorization: Bearer header")
	}
	if err := CheckSameOrigin(c.Request()); err != nil {
		return nil, err
	}
	token := strings.TrimPrefix(authRaw, "Bearer ")
	c.SetCookie(NewSessionCookie(token, SessionCookieSecure(c.Request())))
	return nil, nil
}

// getMe returns information about the current authenticated user.
func (s *Service) getMe(c echo.Context) (interface{}, error) {
	me := c.(*detContext.DetContext).MustGetUser()
	return ByID(context.TODO(), me.ID)
}

func (s *Service) getUsers(c echo.Context) (interface{}, error) {
	userList, err := List(context.TODO())
	if err != nil {
		return nil, err
	}

	var ctx context.Context
	if c.Request() == nil || c.Request().Context() == nil {
		ctx = context.TODO()
	} else {
		ctx = c.Request().Context()
	}

	return AuthZProvider.Get().FilterUserList(ctx,
		c.(*detContext.DetContext).MustGetUser(), userList)
}

func canViewUserErrorHandle(currUser, user model.User, actionErr, notFoundErr error) error {
	ctx := context.TODO()
	if err := AuthZProvider.Get().CanGetUser(ctx, currUser, user); err != nil {
		return authz.SubIfUnauthorized(err, notFoundErr)
	}
	return actionErr
}

func (s *Service) patchUser(c echo.Context) (interface{}, error) {
	if s.extConfig.Enabled() {
		return nil, externalSessionsError
	}
	var ctx context.Context
	if c.Request() == nil || c.Request().Context() == nil {
		ctx = context.TODO()
	} else {
		ctx = c.Request().Context()
	}

	type (
		request struct {
			Password *string `json:"password,omitempty"`
			// OldPassword is the current password, hashed like Password. Users who change their
			// own password must send it.
			OldPassword *string `json:"old_password,omitempty"`
			Active      *bool   `json:"active,omitempty"`
			Admin       *bool   `json:"admin,omitempty"`

			AgentUserGroup *agentUserGroup `json:"agent_user_group,omitempty"`
		}
		response struct {
			message string
		}
	)

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, err
	}

	args := struct {
		Username string `path:"username"`
	}{}
	if err = api.BindArgs(&args, c); err != nil {
		return nil, err
	}

	var params request
	if err = json.Unmarshal(body, &params); err != nil {
		malformedRequestError := echo.NewHTTPError(http.StatusBadRequest, "bad request")
		return nil, malformedRequestError
	}

	userNotFoundErr := api.NotFoundErrs("user", args.Username, false)

	currUser := c.(*detContext.DetContext).MustGetUser()
	user, err := ByUsername(ctx, args.Username)
	switch err {
	case nil:
	case db.ErrNotFound:
		return nil, userNotFoundErr
	default:
		return nil, err
	}

	var toUpdate []string
	if params.Password != nil {
		if err = AuthZProvider.Get().CanSetUsersPassword(ctx, currUser, *user); err != nil {
			return nil, canViewUserErrorHandle(currUser, *user,
				errors.Wrap(forbiddenError, err.Error()), userNotFoundErr)
		}
		err = checkCurrentPasswordHTTP(ctx, currUser, user.ID, OwnPasswordChange, params.OldPassword)
		if err != nil {
			return nil, err
		}

		if err = user.UpdatePasswordHash(*params.Password); err != nil {
			return nil, err
		}
		toUpdate = append(toUpdate, "password_hash")
	}

	if params.Active != nil {
		if err = AuthZProvider.Get().CanSetUsersActive(ctx, currUser, *user, *params.Active); err != nil {
			return nil, canViewUserErrorHandle(currUser, *user,
				errors.Wrap(forbiddenError, err.Error()), userNotFoundErr)
		}

		user.Active = *params.Active
		toUpdate = append(toUpdate, "active")
	}

	if params.Admin != nil {
		if err = AuthZProvider.Get().CanSetUsersAdmin(ctx, currUser, *user, *params.Admin); err != nil {
			return nil, canViewUserErrorHandle(currUser, *user,
				errors.Wrap(forbiddenError, err.Error()), userNotFoundErr)
		}

		user.Admin = *params.Admin
		toUpdate = append(toUpdate, "admin")
	}

	var ug *model.AgentUserGroup
	if pug := params.AgentUserGroup; pug != nil {
		u, pErr := pug.Validate()
		if pErr != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest, pErr.Error())
		}
		ug = u

		if err := AuthZProvider.Get().CanSetUsersAgentUserGroup(ctx, currUser, *user, *ug); err != nil {
			return nil, canViewUserErrorHandle(currUser, *user,
				errors.Wrap(forbiddenError, err.Error()), userNotFoundErr)
		}
	}

	if err := Update(ctx, user, toUpdate, ug); err != nil {
		return nil, err
	}

	return response{
		message: fmt.Sprintf("successfully updated %v", args.Username),
	}, nil
}

// checkCurrentPasswordHTTP wraps CheckCurrentPassword with HTTP status codes, for the legacy
// routes. Their current password is salted and hashed like their passwords.
func checkCurrentPasswordHTTP(
	ctx context.Context, curUser model.User, targetID model.UserID, change OwnAccountChange,
	currentPassword *string,
) error {
	switch err := CheckCurrentPassword(ctx, curUser, targetID, change, currentPassword, true); {
	case err == nil:
		return nil
	case errors.Is(err, ErrCurrentPasswordIncorrect):
		return echo.NewHTTPError(http.StatusForbidden, err.Error())
	case errors.Is(err, ErrCurrentPasswordRequired), errors.Is(err, ErrNoPasswordSignIn):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}

func (s *Service) patchUsername(c echo.Context) (interface{}, error) {
	if s.extConfig.Enabled() {
		return nil, externalSessionsError
	}
	type (
		request struct {
			NewUsername *string `json:"username,omitempty"`
			// OldPassword is the current password, salted and hashed like the passwords of the
			// legacy routes. Users who rename themselves must send it.
			OldPassword *string `json:"old_password,omitempty"`
		}
		response struct {
			message string
		}
	)

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, err
	}

	args := struct {
		Username string `path:"username"`
	}{}
	if err = api.BindArgs(&args, c); err != nil {
		return nil, err
	}

	var params request
	if err = json.Unmarshal(body, &params); err != nil {
		malformedRequestError := echo.NewHTTPError(http.StatusBadRequest, "bad request")
		return nil, malformedRequestError
	}

	user, err := ByUsername(context.TODO(), args.Username)
	if err != nil {
		return nil, err
	}

	currUser := c.(*detContext.DetContext).MustGetUser()

	var ctx context.Context

	if c.Request() == nil || c.Request().Context() == nil {
		ctx = context.TODO()
	} else {
		ctx = c.Request().Context()
	}
	if err = AuthZProvider.Get().CanSetUsersUsername(ctx, currUser,
		*user); err != nil {
		return nil, canViewUserErrorHandle(currUser, *user,
			errors.Wrap(forbiddenError, err.Error()), db.ErrNotFound)
	}

	if params.NewUsername == nil {
		malformedRequestError := echo.NewHTTPError(http.StatusBadRequest, "username is required")
		return nil, malformedRequestError
	}
	err = checkCurrentPasswordHTTP(ctx, currUser, user.ID, OwnUsernameChange, params.OldPassword)
	if err != nil {
		return nil, err
	}

	switch u, uErr := ByUsername(ctx, *params.NewUsername); {
	case uErr == db.ErrNotFound:
	case uErr != nil:
		return nil, uErr
	case u != nil:
		return nil, echo.NewHTTPError(http.StatusBadRequest, "username is taken")
	}

	if err = UpdateUsername(ctx, &user.ID, *params.NewUsername); err != nil {
		return nil, err
	}

	return response{
		message: fmt.Sprintf("successfully updated %v", args.Username),
	}, nil
}

func (s *Service) postUser(c echo.Context) (interface{}, error) {
	if s.extConfig.Enabled() {
		return nil, externalSessionsError
	}
	type (
		request struct {
			Username string `json:"username"`
			Admin    bool   `json:"admin"`
			Active   bool   `json:"active"`

			AgentUserGroup *agentUserGroup `json:"agent_user_group,omitempty"`
		}
		response struct {
			message string
		}
	)

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, err
	}

	var params request
	if err = json.Unmarshal(body, &params); err != nil {
		malformedRequestError := echo.NewHTTPError(http.StatusBadRequest, "bad request")
		return nil, malformedRequestError
	}

	var ug *model.AgentUserGroup
	if pug := params.AgentUserGroup; pug != nil {
		u, pErr := pug.Validate()
		if pErr != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest, pErr.Error())
		}
		ug = u
	}
	params.Username = strings.ToLower(params.Username)

	userToAdd := model.User{
		Username: params.Username,
		Admin:    params.Admin,
		Active:   params.Active,
	}
	currUser := c.(*detContext.DetContext).MustGetUser()

	var ctx context.Context
	if c.Request() == nil || c.Request().Context() == nil {
		ctx = context.TODO()
	} else {
		ctx = c.Request().Context()
	}
	if err = AuthZProvider.Get().CanCreateUser(ctx, currUser, userToAdd,
		ug); err != nil {
		return nil, errors.Wrap(forbiddenError, err.Error())
	}

	_, err = Add(ctx, &userToAdd, ug)
	switch {
	case errors.Is(err, db.ErrDuplicateRecord):
		return nil, api.ErrUserExists
	case err != nil:
		return nil, err
	}

	telemetry.ReportUserCreated(params.Admin, params.Active)

	return response{
		message: fmt.Sprintf("successfully created user: %s", params.Username),
	}, nil
}

func (s *Service) getUserImage(c echo.Context) (interface{}, error) {
	args := struct {
		Username string `path:"username"`
	}{}
	if err := api.BindArgs(&args, c); err != nil {
		return nil, err
	}

	user, err := ByUsername(context.TODO(), args.Username)
	if err != nil {
		return nil, err
	}
	currUser := c.(*detContext.DetContext).MustGetUser()

	var ctx context.Context

	if c.Request() == nil || c.Request().Context() == nil {
		ctx = context.TODO()
	} else {
		ctx = c.Request().Context()
	}
	if err := AuthZProvider.Get().CanGetUsersImage(ctx, currUser,
		*user); err != nil {
		return nil, canViewUserErrorHandle(currUser, *user,
			errors.Wrap(forbiddenError, err.Error()), db.ErrNotFound)
	}

	c.Response().Header().Set("cache-control", "public, max-age=3600")

	return ProfileImage(ctx, args.Username)
}
