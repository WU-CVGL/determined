package internal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/determined-ai/determined/master/internal/cluster"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
)

// What the Echo routes under /api/v1 without a proto share, such as the dynamic pool routes and
// the resource pool access routes: authentication with the permission to read or to update the
// master configuration, and strict JSON request bodies.

var masterConfigRouteUser = func(
	request *http.Request,
) (*model.User, *model.UserSession, error) {
	return user.GetService().UserAndSessionFromRequest(request)
}

var authorizeMasterConfigRoute = func(
	request *http.Request, currentUser *model.User, update bool,
) (permErr error, err error) {
	return authorizeMasterConfigRouteWithProvider(
		cluster.AuthZProvider.Get(), request, currentUser, update,
	)
}

func authorizeMasterConfigRouteWithProvider(
	provider cluster.MiscAuthZ,
	request *http.Request,
	currentUser *model.User,
	update bool,
) (permErr error, err error) {
	if update {
		return provider.CanUpdateMasterConfig(request.Context(), currentUser)
	}
	return provider.CanGetMasterConfig(request.Context(), currentUser)
}

// requireMasterConfigAccess authenticates a direct /api/v1 Echo route and requires the permission
// to update the master configuration, or to read it when update is false. Generic Echo auth exempts
// /api/v1 because normal routes there are authenticated by gRPC interceptors; these routes do not
// pass through gRPC.
func requireMasterConfigAccess(update bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			currentUser, session, err := masterConfigRouteUser(c.Request())
			switch {
			case errors.Is(err, db.ErrNotFound):
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid authentication")
			case err != nil:
				var httpErr *echo.HTTPError
				if errors.As(err, &httpErr) && httpErr.Code == http.StatusUnauthorized {
					return httpErr
				}
				return err
			case !currentUser.Active:
				return echo.NewHTTPError(http.StatusForbidden, "user not active")
			}

			ctx := c.(*detContext.DetContext)
			ctx.SetUser(*currentUser)
			ctx.SetUserSession(*session)
			permErr, err := authorizeMasterConfigRoute(c.Request(), currentUser, update)
			if err != nil {
				return err
			}
			if permErr != nil {
				return echo.NewHTTPError(http.StatusForbidden, permErr.Error())
			}
			return next(c)
		}
	}
}

// decodeJSONBody decodes the body of a request into target: the body must be labeled as JSON, be at
// most maxBytes long and hold exactly one JSON value, with no field that target does not have.
func decodeJSONBody(c echo.Context, maxBytes int64, target interface{}) error {
	body, err := readJSONBody(c, maxBytes)
	if err != nil {
		return err
	}
	return decodeJSONValue(body, target)
}

// readJSONBody returns the body of a request that must be labeled as JSON and be at most maxBytes
// long.
func readJSONBody(c echo.Context, maxBytes int64) ([]byte, error) {
	contentType := c.Request().Header.Get(echo.HeaderContentType)
	if !strings.HasPrefix(strings.ToLower(contentType), echo.MIMEApplicationJSON) {
		return nil, echo.NewHTTPError(
			http.StatusUnsupportedMediaType, "Content-Type must be application/json",
		)
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxBytes)
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			limit := fmt.Sprintf("%d KiB", maxBytes>>10)
			if maxBytes >= 1<<20 {
				limit = fmt.Sprintf("%d MiB", maxBytes>>20)
			}
			return nil, echo.NewHTTPError(
				http.StatusRequestEntityTooLarge, "request body exceeds "+limit,
			)
		}
		return nil, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("reading JSON body: %v", err))
	}
	return body, nil
}

// decodeJSONValue decodes body into target. body must hold exactly one JSON value, with no field
// that target does not have.
func decodeJSONValue(body []byte, target interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return echo.NewHTTPError(http.StatusBadRequest, "JSON body must contain exactly one value")
		}
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
	}
	return nil
}
