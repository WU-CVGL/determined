package internal

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"

	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/poolaccess"
	"github.com/determined-ai/determined/master/pkg/model"
)

// The admin API of resource pool access. Like the dynamic pool routes, these are Echo routes
// without a proto: requireMasterConfigAccess authenticates them, reading needs the permission to
// read the master configuration and writing the permission to update it.

const (
	resourcePoolAccessPath            = "/api/v1/resource-pool-access"
	maxResourcePoolAccessRequestBytes = 64 << 10

	resourcePoolModePublic     = "public"
	resourcePoolModeRestricted = "restricted"

	resourcePoolDefaultCompute = "compute"
	resourcePoolDefaultAux     = "aux"
)

type setResourcePoolAccessRequest struct {
	Mode string `json:"mode"`
}

type resourcePoolAccessUsersRequest struct {
	Usernames []string `json:"usernames"`
}

// resourcePoolAccessUser is a user granted access to a pool.
type resourcePoolAccessUser struct {
	ID       model.UserID `json:"id"`
	Username string       `json:"username"`
	Active   bool         `json:"active"`
	Admin    bool         `json:"admin"`
}

// resourcePoolAccessWorkspaceDefault is a workspace whose default compute or aux pool is the pool.
type resourcePoolAccessWorkspaceDefault struct {
	WorkspaceID int    `json:"workspace_id"`
	Workspace   string `json:"workspace"`
	Kind        string `json:"kind"`
}

// resourcePoolAccessItem is the access of one pool name, as every response shows it.
type resourcePoolAccessItem struct {
	PoolName string `json:"pool_name"`
	// Mode is "restricted" exactly when the pool has a restriction record.
	Mode string `json:"mode"`
	// Exists reports that a resource manager configures a pool with this name in master.yaml or
	// that a dynamic pool is saved with it, in any state.
	Exists bool `json:"exists"`
	// DefaultCompute and DefaultAux report that the pool is a global default pool of a resource
	// manager.
	DefaultCompute    bool                                 `json:"default_compute"`
	DefaultAux        bool                                 `json:"default_aux"`
	WorkspaceDefaults []resourcePoolAccessWorkspaceDefault `json:"workspace_defaults"`
	// Users are the users granted access to the pool, also while it is public.
	Users        []resourcePoolAccessUser `json:"users"`
	RestrictedAt *time.Time               `json:"restricted_at"`
	// RestrictedBy is the username of the administrator who restricted the pool, since the API
	// names users by username. It is null for a public pool and once that user is deleted.
	RestrictedBy *string `json:"restricted_by"`
}

type resourcePoolAccessListResponse struct {
	ResourcePools []resourcePoolAccessItem `json:"resource_pools"`
}

// resourcePoolAccessWriteResponse is a pool's access after a write, with warnings about what the
// access now refuses.
type resourcePoolAccessWriteResponse struct {
	resourcePoolAccessItem
	Warnings []string `json:"warnings"`
}

func (m *Master) registerResourcePoolAccessRoutes() {
	group := m.echo.Group(resourcePoolAccessPath)
	group.GET("", m.listResourcePoolAccess, requireMasterConfigAccess(false))
	group.PUT("/:pool", m.setResourcePoolAccess, requireMasterConfigAccess(true))
	group.POST("/:pool/grant", m.grantResourcePoolAccess, requireMasterConfigAccess(true))
	group.POST("/:pool/revoke", m.revokeResourcePoolAccess, requireMasterConfigAccess(true))
}

// listResourcePoolAccess lists every pool name that is a known pool or has access records:
// known pools without records are public, and names with records but no pool are orphans.
func (m *Master) listResourcePoolAccess(c echo.Context) error {
	state, err := m.readResourcePoolAccess(c.Request().Context())
	if err != nil {
		return err
	}
	names := state.names()
	items := make([]resourcePoolAccessItem, 0, len(names))
	for _, name := range names {
		items = append(items, state.item(name))
	}
	return c.JSON(http.StatusOK, resourcePoolAccessListResponse{ResourcePools: items})
}

// setResourcePoolAccess restricts a pool or makes it public. Making a pool public keeps its
// grants, which apply again when it is restricted again. Any non-empty name is accepted, so that a
// pool can be restricted before it is created.
func (m *Master) setResourcePoolAccess(c echo.Context) error {
	pool, err := resourcePoolAccessPoolParam(c)
	if err != nil {
		return err
	}
	var request setResourcePoolAccessRequest
	if err := decodeJSONBody(c, maxResourcePoolAccessRequestBytes, &request); err != nil {
		return err
	}
	ctx := c.Request().Context()
	admin := c.(*detContext.DetContext).MustGetUser()
	var changed bool
	var action string
	switch request.Mode {
	case resourcePoolModeRestricted:
		changed, err = poolaccess.Restrict(ctx, pool, admin.ID)
		action = fmt.Sprintf("restricted %q", pool)
	case resourcePoolModePublic:
		changed, err = poolaccess.MakePublic(ctx, pool)
		action = fmt.Sprintf("made %q public", pool)
	default:
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
			"mode must be %q or %q", resourcePoolModePublic, resourcePoolModeRestricted,
		))
	}
	if err != nil {
		return err
	}
	logResourcePoolAccessWrite(admin, action, changed)
	return m.resourcePoolAccessWritten(c, pool)
}

func (m *Master) grantResourcePoolAccess(c echo.Context) error {
	return m.changeResourcePoolGrants(c, true)
}

func (m *Master) revokeResourcePoolAccess(c echo.Context) error {
	return m.changeResourcePoolGrants(c, false)
}

// changeResourcePoolGrants grants or revokes access to a pool, whether it is public or restricted.
// An unknown username refuses the whole request before anything is written.
func (m *Master) changeResourcePoolGrants(c echo.Context, grant bool) error {
	pool, err := resourcePoolAccessPoolParam(c)
	if err != nil {
		return err
	}
	var request resourcePoolAccessUsersRequest
	if err := decodeJSONBody(c, maxResourcePoolAccessRequestBytes, &request); err != nil {
		return err
	}
	ctx := c.Request().Context()
	userIDs, usernames, err := resourcePoolAccessUsers(ctx, request.Usernames)
	if err != nil {
		return err
	}
	admin := c.(*detContext.DetContext).MustGetUser()
	var changedIDs []model.UserID
	if grant {
		changedIDs, err = poolaccess.Grant(ctx, pool, userIDs, admin.ID)
	} else {
		changedIDs, err = poolaccess.Revoke(ctx, pool, userIDs)
	}
	if err != nil {
		return err
	}
	// The log names the users whose access changed, or the requested ones when none did.
	logged := userIDs
	if len(changedIDs) > 0 {
		logged = changedIDs
	}
	names := make([]string, 0, len(logged))
	for _, id := range logged {
		names = append(names, usernames[id])
	}
	sort.Strings(names)
	action := fmt.Sprintf("granted %q to %s", pool, strings.Join(names, ", "))
	if !grant {
		action = fmt.Sprintf("revoked %q from %s", pool, strings.Join(names, ", "))
	}
	logResourcePoolAccessWrite(admin, action, len(changedIDs) > 0)
	return m.resourcePoolAccessWritten(c, pool)
}

func logResourcePoolAccessWrite(admin model.User, action string, changed bool) {
	if !changed {
		action += " (unchanged)"
	}
	log.Infof("resource pool access: %q %s", admin.Username, action)
}

// resourcePoolAccessWritten answers a write with the pool's access as it is now. Every write is
// idempotent, so a client that gets an error here can repeat the request.
func (m *Master) resourcePoolAccessWritten(c echo.Context, pool string) error {
	state, err := m.readResourcePoolAccess(c.Request().Context())
	if err != nil {
		return err
	}
	item := state.item(pool)
	return c.JSON(http.StatusOK, resourcePoolAccessWriteResponse{
		resourcePoolAccessItem: item,
		Warnings:               resourcePoolAccessWarnings(item),
	})
}

// resourcePoolAccessWarnings names what an item's access refuses that an administrator may not
// expect: a setting for a name that is not a pool, and the submissions that omit a pool and are
// refused because the restricted pool is a default.
func resourcePoolAccessWarnings(item resourcePoolAccessItem) []string {
	warnings := []string{}
	pool := item.PoolName
	if !item.Exists {
		warnings = append(warnings, fmt.Sprintf(
			"no resource pool named %q exists; the setting applies to a pool created with this name",
			pool,
		))
	}
	if item.Mode != resourcePoolModeRestricted {
		return warnings
	}
	refused := fmt.Sprintf(
		"that omit resources.resource_pool are refused for users without a grant on %q", pool,
	)
	if item.DefaultCompute {
		warnings = append(warnings, fmt.Sprintf(
			"%q is the cluster's default compute pool: submissions %s", pool, refused,
		))
	}
	if item.DefaultAux {
		warnings = append(warnings, fmt.Sprintf(
			"%q is the cluster's default aux pool: submissions %s", pool, refused,
		))
	}
	for _, workspaceDefault := range item.WorkspaceDefaults {
		warnings = append(warnings, fmt.Sprintf(
			"%q is the default %s pool of workspace %q: submissions there %s",
			pool, workspaceDefault.Kind, workspaceDefault.Workspace, refused,
		))
	}
	return warnings
}

// resourcePoolAccessState is everything that the items of the admin API show.
type resourcePoolAccessState struct {
	known             map[string]bool
	defaultCompute    map[string]bool
	defaultAux        map[string]bool
	workspaceDefaults map[string][]resourcePoolAccessWorkspaceDefault
	restrictions      map[string]poolaccess.RestrictionRecord
	users             map[string][]resourcePoolAccessUser
}

func (m *Master) readResourcePoolAccess(ctx context.Context) (*resourcePoolAccessState, error) {
	restrictions, grants, err := poolaccess.List(ctx)
	if err != nil {
		return nil, err
	}
	// The resource managers list Ready pools only, so dynamic pools are read from their records.
	dynamicPools, err := m.db.ListDynamicResourcePools(ctx, "")
	if err != nil {
		return nil, err
	}
	workspaceDefaults, err := listWorkspaceDefaultPools(ctx)
	if err != nil {
		return nil, err
	}

	state := &resourcePoolAccessState{
		known:             map[string]bool{},
		defaultCompute:    map[string]bool{},
		defaultAux:        map[string]bool{},
		workspaceDefaults: map[string][]resourcePoolAccessWorkspaceDefault{},
		restrictions:      map[string]poolaccess.RestrictionRecord{},
		users:             map[string][]resourcePoolAccessUser{},
	}
	// The configuration holds the pool named default when master.yaml omits resource_pools.
	for _, rmConfig := range m.config.ResourceManagers() {
		for _, pool := range rmConfig.ResourcePools {
			state.known[pool.PoolName] = true
		}
		compute, aux := resourceManagerDefaultPools(rmConfig.ResourceManager)
		if compute != "" {
			state.defaultCompute[compute] = true
		}
		if aux != "" {
			state.defaultAux[aux] = true
		}
	}
	for _, record := range dynamicPools {
		state.known[record.PoolName] = true
	}
	for _, row := range workspaceDefaults {
		state.workspaceDefaults[row.PoolName] = append(
			state.workspaceDefaults[row.PoolName], resourcePoolAccessWorkspaceDefault{
				WorkspaceID: row.WorkspaceID, Workspace: row.Workspace, Kind: row.Kind,
			},
		)
	}
	for _, restriction := range restrictions {
		state.restrictions[restriction.PoolName] = restriction
	}
	for _, grant := range grants {
		state.users[grant.PoolName] = append(state.users[grant.PoolName], resourcePoolAccessUser{
			ID: grant.UserID, Username: grant.Username, Active: grant.Active, Admin: grant.Admin,
		})
	}
	return state, nil
}

// names returns, sorted, the known pools and the names that have access records.
func (s *resourcePoolAccessState) names() []string {
	set := map[string]bool{}
	for name := range s.known {
		set[name] = true
	}
	for name := range s.restrictions {
		set[name] = true
	}
	for name := range s.users {
		set[name] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *resourcePoolAccessState) item(pool string) resourcePoolAccessItem {
	item := resourcePoolAccessItem{
		PoolName:          pool,
		Mode:              resourcePoolModePublic,
		Exists:            s.known[pool],
		DefaultCompute:    s.defaultCompute[pool],
		DefaultAux:        s.defaultAux[pool],
		WorkspaceDefaults: s.workspaceDefaults[pool],
		Users:             s.users[pool],
	}
	if item.WorkspaceDefaults == nil {
		item.WorkspaceDefaults = []resourcePoolAccessWorkspaceDefault{}
	}
	if item.Users == nil {
		item.Users = []resourcePoolAccessUser{}
	}
	if restriction, ok := s.restrictions[pool]; ok {
		item.Mode = resourcePoolModeRestricted
		restrictedAt := restriction.RestrictedAt
		item.RestrictedAt = &restrictedAt
		item.RestrictedBy = restriction.RestrictedByUsername
	}
	return item
}

// resourceManagerDefaultPools returns the global default pools of a resource manager, the
// settings that checkIfRMDefaultsAreUnbound reads.
func resourceManagerDefaultPools(rmConfig *config.ResourceManagerConfig) (compute, aux string) {
	deref := func(pool *string) string {
		if pool == nil {
			return ""
		}
		return *pool
	}
	switch {
	case rmConfig == nil:
		return "", ""
	case rmConfig.AgentRM != nil:
		return rmConfig.AgentRM.DefaultComputeResourcePool, rmConfig.AgentRM.DefaultAuxResourcePool
	case rmConfig.KubernetesRM != nil:
		return rmConfig.KubernetesRM.DefaultComputeResourcePool,
			rmConfig.KubernetesRM.DefaultAuxResourcePool
	case rmConfig.DispatcherRM != nil:
		return deref(rmConfig.DispatcherRM.DefaultComputeResourcePool),
			deref(rmConfig.DispatcherRM.DefaultAuxResourcePool)
	case rmConfig.PbsRM != nil:
		return deref(rmConfig.PbsRM.DefaultComputeResourcePool),
			deref(rmConfig.PbsRM.DefaultAuxResourcePool)
	default:
		return "", ""
	}
}

type workspaceDefaultPoolRow struct {
	PoolName    string `bun:"pool_name"`
	WorkspaceID int    `bun:"workspace_id"`
	Workspace   string `bun:"workspace"`
	Kind        string `bun:"kind"`
}

// listWorkspaceDefaultPools returns the default compute and aux pools of every workspace that
// sets them, ordered by workspace.
func listWorkspaceDefaultPools(ctx context.Context) ([]workspaceDefaultPoolRow, error) {
	rows := []workspaceDefaultPoolRow{}
	if err := db.Bun().NewRaw(`
SELECT default_compute_pool AS pool_name, id AS workspace_id, name AS workspace, ? AS kind
FROM workspaces WHERE COALESCE(default_compute_pool, '') <> ''
UNION ALL
SELECT default_aux_pool AS pool_name, id AS workspace_id, name AS workspace, ? AS kind
FROM workspaces WHERE COALESCE(default_aux_pool, '') <> ''
ORDER BY workspace, workspace_id, kind DESC`,
		resourcePoolDefaultCompute, resourcePoolDefaultAux,
	).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("listing workspace default resource pools: %w", err)
	}
	return rows, nil
}

// resourcePoolAccessUsers resolves usernames to user IDs, in request order and without
// duplicates. When any username is unknown, it refuses the request and names all of them.
func resourcePoolAccessUsers(
	ctx context.Context, usernames []string,
) ([]model.UserID, map[model.UserID]string, error) {
	if len(usernames) == 0 {
		return nil, nil, echo.NewHTTPError(
			http.StatusBadRequest, "usernames must name at least one user",
		)
	}
	for _, username := range usernames {
		if username == "" {
			return nil, nil, echo.NewHTTPError(
				http.StatusBadRequest, "usernames must not be empty",
			)
		}
	}
	var users []model.User
	if err := db.Bun().NewSelect().Model(&users).Column("id", "username").
		Where("username IN (?)", bun.In(usernames)).Scan(ctx); err != nil {
		return nil, nil, fmt.Errorf("looking up users: %w", err)
	}
	byUsername := make(map[string]model.UserID, len(users))
	for _, u := range users {
		byUsername[u.Username] = u.ID
	}
	seen := map[string]bool{}
	var userIDs []model.UserID
	var unknown []string
	for _, username := range usernames {
		if seen[username] {
			continue
		}
		seen[username] = true
		if id, ok := byUsername[username]; ok {
			userIDs = append(userIDs, id)
		} else {
			unknown = append(unknown, username)
		}
	}
	if len(unknown) > 0 {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf(
			"unknown users: %s; nothing was changed", strings.Join(unknown, ", "),
		))
	}
	names := make(map[model.UserID]string, len(byUsername))
	for username, id := range byUsername {
		names[id] = username
	}
	return userIDs, names, nil
}

// resourcePoolAccessPoolParam returns the pool name in the path. Echo routes on the escaped path
// when it differs from the decoded one, as for a name with "/" sent as %2F, and then leaves the
// parameter escaped.
func resourcePoolAccessPoolParam(c echo.Context) (string, error) {
	pool := c.Param("pool")
	if c.Request().URL.RawPath != "" {
		unescaped, err := url.PathUnescape(pool)
		if err != nil {
			return "", echo.NewHTTPError(
				http.StatusBadRequest, fmt.Sprintf("invalid resource pool name: %v", err),
			)
		}
		pool = unescaped
	}
	if pool == "" {
		return "", echo.NewHTTPError(http.StatusBadRequest, "a resource pool name is required")
	}
	return pool, nil
}
