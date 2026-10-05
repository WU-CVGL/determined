# Pool ACL v1: per-user resource pool access (public by default)

Status: design for review. The implementation follows in this PR once the design is approved. A small TensorBoard identity fix (D18) ships and deploys first as its own PR; a continue identity fix (D16) is independent (sections 11 and 12).

Base: main, which includes PR #35 (dynamic pools and adopt, squash-merged as 7d0982e4b). It ships as its own PR after the 0.41.0 release (#36).

Line references. Every `file:line` reference is to commit 3ce0d87b9, the head of #35. Its tree is identical to main 7d0982e4b, so every reference holds at both.

Paths. Paths that do not start with master/, harness/ or docs/ are relative to master/internal. Exceptions: agent_resource_manager.go and dynamic_pools.go (master/internal/rm/agentrm), jobservice.go (master/internal/job/jobservice), pkg/tasks/task.go (master/pkg/tasks), tensorboard-entrypoint.sh (master/static/srv), migration-create.sh and migration-move-to-top.sh (master/static/migrations), ResourcepoolDetail.tsx (webui/react/src/pages/ResourcePool), go.mod (repository root), job.py and resource_pool.py (harness/determined/cli), and collection.go (go-pg/migrations v8.1.0 module). master.yaml means the master's configuration file.

Terms. "Admission" is the decision to accept new work into a pool. S1-S7 are the admission check sites, W1 is the workspace-default check and L1 is the list filter (section 5). M1-M4 and A1 are error messages (section 13). D-numbers are decisions (section 15), T-numbers tests and R-numbers risks (sections at the end). "The MCP server" is the cluster's job-submission service for agents, a separate project that calls this master's REST API.

## 1. Rule

A user may start new work in pool P when any of these holds:
  - The user passes the admin predicate: cluster.AuthZProvider.Get().CanUpdateMasterConfig returns no permission error (basic mode: users.admin). It is checked first and reads no ACL table, so admins keep working when the tables cannot be read.
  - P has no restriction record. Such a pool is public. This is the rule for every pool (existing, adopted, newly created, static or dynamic), not a fallback.
  - P is restricted and the user has a grant on P.

Everything else is denied:
  - restricted with no grant for this user, including restricted with no grants at all (admins only);
  - the access tables cannot be read (Unavailable; never treated as "no record");
  - an empty pool name (programming guard, Internal).

The check never picks another pool, and nothing re-routes a refused request. The ACL is not RBAC: it has users, pools and grants only, and works the same in basic and RBAC mode.

## 2. Data model and migration

One migration, `master/static/migrations/<ts>_add-resource-pool-access.tx.up.sql`, created with migration-create.sh. `<ts>` must sort after 20261005000000 (the #35 spec migration) and must be the newest migration when the PR merges and when it is deployed, because go-pg/migrations v8.1.0 skips every migration at or below the database's current version (collection.go:465; R1). If #35 or main gains a later migration first, run migration-move-to-top.sh on this one. No down file: master/static/migrations/README.md says down migrations are not supported, and an older binary ignores the tables (section 3).

```sql
CREATE TABLE resource_pool_restrictions (
    pool_name     TEXT PRIMARY KEY CHECK (pool_name <> ''),
    restricted_by INT REFERENCES users(id) ON DELETE SET NULL,
    restricted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE resource_pool_grants (
    pool_name  TEXT NOT NULL CHECK (pool_name <> ''),
    user_id    INT  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    granted_by INT  REFERENCES users(id) ON DELETE SET NULL,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pool_name, user_id)
);
```

  - No seed and no rows for public pools. A restriction row's presence makes a pool restricted; "set public" deletes it. There is no mode column, so public has one representation.
  - Grants have no foreign key to the restriction row. A grant on a public pool is stored and has no effect until the pool is restricted. This gives the gap-free switch (grant, then restrict) and lets an admin prepare a pool before it exists. Grants survive "set public" (D17).
  - Rows are keyed by pool name: no foreign key to dynamic_resource_pools and no cluster name. Pool names are globally unique: dynamic_resource_pools.pool_name is UNIQUE, a dynamic pool may not take a static pool's name (ErrStaticResourcePoolConflict, dynamic_pools.go:63, raised at :169; the Echo create refuses it at core_resource_pools.go:154-158; startup refuses a conflicting row at dynamic_pools.go:787-791), adopt keeps the name, and rp_workspace_bindings is keyed the same way.
  - Access is never part of a dynamic pool's config or spec. An access change never touches the pool registry, agents, spec_hash, revision, or a restart.
  - Grants are stored by user ID, because usernames can change; the API speaks usernames.
  - No optimistic concurrency. Each write is one idempotent statement, the last writer wins, and every write is logged at INFO with the admin and the change.

Code: one new package, master/internal/poolaccess, holding the check (section 4) and a small bun store, like db/postgres_rp_workspace_bindings.go.
  - restrictionsFor(ctx, userID, pools []string) (map[string]bool, error), one query:

    ```sql
    SELECT r.pool_name, (g.user_id IS NOT NULL) AS granted
    FROM resource_pool_restrictions r
    LEFT JOIN resource_pool_grants g ON g.pool_name = r.pool_name AND g.user_id = ?
    WHERE r.pool_name IN (?)
    ```

    The map holds only restricted pools (value = granted); an absent pool is public. A query error is returned as an error, never as an empty map. An empty pools slice returns an empty map without a query (bun cannot render an empty IN list).
  - Restrict(ctx, pool, byUserID) (changed bool, err): INSERT ... ON CONFLICT DO NOTHING.
  - MakePublic(ctx, pool) (changed bool, err): DELETE ... WHERE pool_name = ?.
  - Grant(ctx, pool, userIDs, byUserID) (added []model.UserID, err): INSERT ... ON CONFLICT DO NOTHING RETURNING user_id.
  - Revoke(ctx, pool, userIDs) (removed []model.UserID, err): DELETE ... RETURNING user_id.
  - List(ctx): restrictions, and grants with username, active and admin, for the admin list.

## 3. Upgrade and pool lifecycle

Upgrade: the migration creates two empty tables, so no pool changes who may use it until an admin restricts it. There is no post-upgrade step. These changes apply to public pools too (a-d are in the release note; e and f are not user-visible regressions):
  - a. Every non-admin admission (S1-S7), pool list (L1) and change of a workspace default to a non-empty pool (W1) reads the access tables. A database error, or missing tables (R1), fails those requests with Unavailable (M2). Admins are unaffected.
  - b. A job-queue move with an empty target is rejected with InvalidArgument (S7, D8).
  - c. CreateExperiment with validate_only now resolves the pool an invariant config policy sets (S3). A policy pool that is not Ready or not available to the workspace now fails validation with InvalidArgument. Before, validation passed, and the real create failed in newExperiment with Internal.
  - d. In basic mode, only the workspace owner or an admin may change a workspace's default pools (W1). Before, any user could.
  - e. A zero-slot experiment that omits its pool while the global aux default is not Ready now fails before it is saved, instead of being saved and then failing to start (S3). ValidateOnly still passes it.
  - f. CreateExperimentResponse.Config carries the resolved resource_pool.

Lifecycle. Access follows the pool name; nothing reads or deletes records automatically.
  - New dynamic pools (POST /api/v1/resource-pools/dynamic) are public once Ready (D12). Create has no access field. To avoid a public window, restrict the name before creating the pool; the admin API accepts a name with no pool (section 8). Restricting while the pool is Pending also works, because ResolveResourcePool accepts only names in the ready list (agent_resource_manager.go:546-567).
  - Adopt never reads or writes access records, so an adopted pool keeps its restriction and grants.
  - Delete and re-create, rollback of a conversion, re-adopt: the same name gets its records back.
  - Rename of a master.yaml pool: the new name is a new, public pool. The old name's records stay as orphans (exists=false in the admin list); clean them with set public and revoke.
  - Rollback to a binary without the ACL: the tables are ignored and every pool is public. Rolling forward again restores the restrictions as they were.
  - Restoring a pg_dump taken before the ACL: the migration runs again on empty tables, so every pool is public. Save `det rp access list --json` with each pg_dump and reapply from it.

## 4. The central check

Package master/internal/poolaccess. It imports db, cluster, model and grpc status. It adds to FilterResourcePools, workspace bindings and the RBAC checks; it replaces none of them.

```go
// CanUseResourcePool returns nil when user may start new work in pool, PermissionDenied when
// not, and Unavailable or Internal when access could not be decided. pool must be the final,
// resolved name.
func CanUseResourcePool(ctx context.Context, user model.User, pool string) error

// UsablePools returns, for each name, whether user may use it (admins: all true, no read).
// One query, no cache; an error is returned, never a partial answer.
func UsablePools(ctx context.Context, user model.User, pools []string) (map[string]bool, error)
```

CanUseResourcePool:
  1. pool == "": Internal (M4).
  2. cluster.AuthZProvider.Get().CanUpdateMasterConfig(ctx, &user): an error -> Internal; no permission error -> nil (admin).
  3. `m, err := ReadRestrictions(ctx, user.ID, []string{pool})`. `var ReadRestrictions = restrictionsFor` is exported as a documented test hook, like the request-user and authorize hooks at core_resource_pools.go:25-37. err -> Unavailable (M2). This return comes before any look at m, so a failed read never reaches the public branch.
  4. `granted, restricted := m[pool]`: not restricted -> nil; granted -> nil; otherwise log one INFO line (user, pool) and return PermissionDenied (M1).

Properties:
  - The user is a model.User value: there is no nil-user path and no "no user = allowed" branch.
  - No cache. Each call reads the database, so a restriction or revocation applies from the next request.
  - Linearization point: an admission is decided when CanUseResourcePool reads the tables. A restriction committed after that read does not stop that request.
  - Callers pass the acting request user from grpcutil.GetUser(ctx) (D4). In basic mode a non-admin acts only on their own jobs, so for non-admins the actor is the owner. Requests made from inside a task are checked as the task's user (section 11).

Package doc invariant: CanUseResourcePool is called only from S1-S7 and W1, UsablePools only from L1. Nothing at or below task.DefaultService.StartAllocation or rm.Allocate calls either. Continuations and system tasks are exempt by design (section 6).

## 5. Check sites

All at the user-operation layer, after the final pool is known, before anything is persisted.

### S1 getCommandLaunchParams (api_command.go:61)
  - Where: after :145-146 set config.Resources.ResourcePool and Slots from the resolved poolName; before CheckNTSCConstraints (:162) and getTaskSessionToken (:167, which writes a user_sessions row). CanUseResourcePool(ctx, *userModel, poolName.String()), userModel from :74.
  - Nothing changes the pool after :145: NTSC invariant overrides are still a TODO (:153), and a template cannot override the pool because :145 overwrites it.
  - Covers LaunchCommand (:342, call at :350), LaunchShell (api_shell.go:210, call at :218), LaunchNotebook (api_notebook.go:267, call at :275) including Preview (:308), and LaunchTensorboard (api_tensorboard.go:215, call at :231; zero-slot, so an omitted pool resolves to the aux default).

### S2 getGenericTaskLaunchParameters (api_generic_tasks.go:74)
  - Where: after :138-139 set RawResourcePool from the resolved poolName; before validateGenericTaskScheduling (:152) and getTaskSessionToken (:169). CanUseResourcePool(ctx, *userModel, poolName.String()).
  - Covers CreateGenericTask (:243): a plain create, a fork (ForkedFrom, :267; the forked config is resolved again) and a child (ParentId, InheritContext, :302), all before the persist transaction (RunInTx at :327).

### S3 parseCreateExperiment (core_experiment.go:277)
  - Where: right after `config = *configWithInvariantOverrides` (:380), only when !req.GetUnmanaged(), before getTaskSessionToken (:412):

    ```go
    if owner == nil { return codes.Internal }   // never allowed
    res := config.Resources()
    final, err := m.rm.ResolveResourcePool(rm.ResourcePoolName(res.ResourcePool()), workspaceID, res.SlotsPerTrial())
    if err != nil { return status.Errorf(codes.InvalidArgument, "invalid resource configuration: %s", err) }
    if err := poolaccess.CanUseResourcePool(ctx, *owner, final.String()); err != nil { return err }
    res.SetResourcePool(final.String()); config.SetResources(res)   // write-back
    ```

  - Why after :380: an invariant config policy can set resources.resource_pool (configpolicy/task_config_policy.go:182-215 merges the policy over the user's config), so the pool resolved at core_experiment.go:319 is not final.
  - Error code: ResolveResourcePool returns plain errors, so the failure is wrapped as InvalidArgument explicitly, the code the first resolution at :319 gets through m.ResolveResources (spec_util.go:38-41). errors.Wrapf would surface as Unknown.
  - The write-back is load-bearing. newExperiment resolves again (experiment.go:109). With the resolved name written back, that is an explicit name, which resolves to itself or fails (agent_resource_manager.go:546-572). So the checked pool is the pool AddExperiment persists (experiment.go:146), even if a workspace default changes in between. Side effects: deltas c, e and f (section 3). For e: the explicit name is refused while not Ready (experiment.go:109-114), so CreateExperiment returns Internal (api_experiment.go:1673-1674) before AddExperiment; before, the omitted name took the default branch without a ready check (agent_resource_manager.go:524-532) and the row was saved, then e.Start() failed (api_experiment.go:1678-1680). ValidateOnly returns at :1655, before newExperiment.
  - The user checked is the parameter `owner`, which both managed callers set to the request user (api_experiment.go:1466-1469, :1622-1624). If D16 lands first and adds an `actor` parameter, S3 uses `actor`.
  - Covers CreateExperiment (api_experiment.go:1592): create, fork and clone (ParentId), ValidateOnly (:1655; D9), Activate (S3a), templates (merged at core_experiment.go:293-300) and invariant policies. Covers ContinueExperiment (api_experiment.go:1441, call at :1466): newExperiment with ID != 0 never persists and the continue transaction (:1482-1567) runs after S3, so a denial leaves nothing behind. grpc-go v1.64.1 (go.mod:54) status.FromError unwraps errors, so the fmt.Errorf wrap at api_experiment.go:1472 keeps PermissionDenied or Unavailable.
  - PutExperiment (:1739) is unmanaged-only and skips S3, like every unmanaged path; none allocates. An unmanaged experiment becomes managed only through ContinueExperiment.

### S3a One admission decision for create or continue with activation
  - CreateExperiment (:1683) and ContinueExperiment (:1577) call a.ActivateExperiment, the RPC handler, after the experiment is saved and started, and rewrap its error as Internal (:1685, :1579). A second ACL read there could fail after the first succeeded and leave a persisted PAUSED experiment.
  - Change: both call e.ActivateExperiment() on the *internalExperiment returned by newExperiment (api_experiment.go:1672, :1477). The handler's re-authorization is already done in the same request on the same experiment: CanEditExperiment at :1667 (create with Activate) and :1449 (continue). e.Start() registers the experiment (experiment.go:218-233). Under RBAC, CanEditExperiment (experiment/authz_rbac.go:278-294) checks UPDATE_EXPERIMENT on the same workspace.
  - Effect: one decision per request, at S3's read. The MCP server, which always creates with activate:true, gets PermissionDenied or Unavailable before anything exists.

### S4 apiServer.ActivateExperiment (api_experiment.go:847), the external RPC only
  - Where: after the registry Load (:855), before e.ActivateExperiment() (:859): admitExperimentPool(ctx, e), a package-level function: grpcutil.GetUser(ctx), then CanUseResourcePool(ctx, *user, e.ResourcePool()).
  - Add `ResourcePool() string` to experiment.Experiment (experiment/experiment_iface.go:48-60). internalExperiment already implements it (experiment_job_service.go:86) from its live config, which reflects job-queue moves. experimentMock embeds the interface.

### S5 experiment.ActivateExperiments (experiment/bulk_action.go:215)
  - Add a required parameter `admit func(context.Context, Experiment) error`, called for each ref before ref.ActivateExperiment() (:244). An error becomes that experiment's ExperimentActionResult.Error and the experiment is not activated. A nil admit returns an error; it never means "allow". The callback keeps poolaccess out of the experiment package.
  - Callers pass admitExperimentPool: apiServer.ActivateExperiments (api_experiment.go:865, call at :868) and pauseResumeAction (api_runs.go:992), which serves ResumeRuns (:899).

### S6 UnpauseGenericTask (api_generic_tasks.go:838)
  - Where: in the `len(plan) == 0` branch, after the state checks, before makeGenericTaskResumePlan (:908, which persists the plan). For each member of tasksToResume in state Paused, read getGenericTaskSpec (:977) and check spec.GenericTaskConfig.Resources.ResourcePool(), the pool the resume allocates in (generic_task_resume.go:378).
  - Any denial returns before anything is written. A read error or a nil spec fails the request; a member is never skipped.
  - A retried, already persisted plan (len(plan) > 0, :877-893) is a continuation (D7). The check is not inside runGenericTaskResume, which recoverGenericTaskResumes runs at startup with no user (generic_task_resume.go:420-439). Resume reuses each member's stored Owner and token (:303, :372).

### S7 updateJobQueueAuthorized (api_job.go:137)
  - Where: in the per-update preflight loop, after authorize (:146), for QueueControl_ResourcePool only. Target "" -> InvalidArgument (M3; D8): today jobservice.go:205-209 only logs it and setRP("") resolves to a default pool (experiment.go:989), an unchecked re-route. Otherwise CanUseResourcePool(ctx, curUser, target). Any failure rejects the whole batch before apply.
  - An explicit target resolves to itself in setRP (experiment.go:989-997), so the checked name is the name used.
  - Priority and weight updates are never checked (section 6). Commands and generic tasks already refuse pool moves (command/command_job_service.go:98, generic_task_job.go:244).

### W1 Workspace default pools (D11)
A workspace default decides the pool of every submission in that workspace that omits one (GetDefaultPoolsForWorkspace, db/postgres_rp_workspace_bindings.go:275; workspace defaults win over global ones in agent_resource_manager.go:519-544). Two changes:
  - Who may change them. Basic mode: CanSetWorkspacesDefaultPools (workspace/authz_basic_impl.go:186-191) returns nil for everyone today. It becomes owner-or-admin, like CanSetWorkspacesName (:75-84). PatchWorkspace already calls it (api_workspace.go:722-727). Permissive mode enforces the basic answer (workspace/authz_permissive.go:150-156) and follows. RBAC mode keeps SET_WORKSPACE_DEFAULT_RESOURCE_POOL (workspace/authz_rbac.go:354-368).
  - Which pools. In every mode, a new non-empty default must pass CanUseResourcePool for the setter: admins and public pools pass; a restricted pool needs the setter's grant. Denial: PermissionDenied (M1).
      - PatchWorkspace (api_workspace.go:676): in the default-pools block, after each availability check (:742-746 compute, :750-754 aux), when the value is non-empty and differs from the current one (currWorkspace.DefaultComputePool / DefaultAuxPool, loaded through GetWorkspaceByID at :256). Re-sending the current value is not checked, so a client that echoes an existing restricted default while changing the other field is not refused. "" (unset) is never checked.
      - PostWorkspace (api_workspace.go:473): after the authz and validation (:481-501), before the model.Workspace literal (:503-506) and the insert transaction (:510), for each non-empty req.DefaultComputePool / DefaultAuxPool. The creator becomes the owner (:504), so only the pool check applies. PostWorkspace's missing availability check is upstream and stays.
  - Result in basic mode: a restricted pool becomes a workspace default only through an admin, or through the workspace's owner holding a grant. Nobody else can point a shared workspace at a restricted pool. Under RBAC, the setter needs SET_WORKSPACE_DEFAULT_RESOURCE_POOL on the workspace and must pass the pool-use check.

### L1 GetResourcePools list filter (api_resourcepool.go:45)
Not an admission check. See section 7.

## 6. Not checked: continuations, system tasks and management of accepted work

Exempt internal paths (they never call the check):
  - Experiment restore: restore.go:60-128 (resolves again at :78, owner from the DB at :102-106). Startup: core.go:1423.
  - Trial allocations: trial.go:438 (restored) and :502 (new), including searcher-created trials (experiment.go:655-662, reused by continueTrials) and restored trials (restore.go:197); restarts after failure or preemption; PatchRP after an admitted move (trial.go:250-255).
  - Command, shell, notebook and TensorBoard restore: command/command.go:161, from RestoreAllCommands (core.go:1443).
  - Generic task restore: core.go:879-960, StartAllocation at :938 (startup :1448). Generic task resume: recoverGenericTaskResumes (startup core.go:1451) and a retried persisted plan (S6).
  - Checkpoint GC (D5): checkpoint_gc.go:101 resolves ResolveResourcePool("", -1, 0), the global aux default; StartAllocation at :175. A system task, exempt for every trigger, including when an admin restricts the global aux default. Add a comment at :101: "System task: exempt from the resource pool ACL by design; see the poolaccess package doc."

Management of accepted work is not admission. Queued experiments, searcher-created trials, restarts, and changes to an accepted experiment's priority, weight or max_slots are not checked; they keep their existing permissions (D15). max_slots needs a word, because raising it lets accepted work use more slots. A raise check in PatchExperiment cannot be made sound: the decision would read the stored value, and the write comes later (load at api_experiment.go:1076, SetGroupMaxSlots at :1184, SaveExperimentConfig at :1200), with no lock in between. Example: the check reads 64 and allows the owner's 32 as a lowering; an admin's PATCH then sets 0; the owner's write of 32 lands, a raise over 0 that was never checked. Making read and write atomic needs the experiment's own synchronization, which is out of scope for v1. So max_slots stays owner-or-admin (CanSetExperimentsMaxSlots, experiment/authz_basic_impl.go:102-107).

Revocation. Restricting a pool or revoking a grant affects S1-S7 and W1 from the next request. Nothing is killed, paused or moved. Everything above keeps running, and existing workspace defaults stay. A user without access can no longer submit, activate, unpause, continue, fork or clone into the pool, move a job into it, or make it a new workspace default. Work submitted later from inside a running task is a new admission and is refused (D6). The pool disappears from that user's lists (section 7); their own experiment and task pages still work. Admins handle running work explicitly: pause it (S4 then refuses a non-granted reactivation), kill it, or move it to another pool (S7). Setting max_slots to 0 is not a revocation tool, because the owner can raise it again (R5).

## 7. List filtering (L1)

GetResourcePools (api_resourcepool.go:45):
  - Between FilterResourcePools (:70-73) and the sort (:75), drop every pool for which UsablePools is false. The Unbound subset (:78-84) and Paginate (:86) then see only usable pools, so totals are right.
  - On a read error, return the error, never the unfiltered list. Admins see every pool without a read.

Every reader of this endpoint then shows only usable pools: the WebUI launch forms, HyperparameterSearchModal, ManageJob "move to pool", the cluster pages, the MCP server's inventory, the SDK and the CLI. Pool names are not secret, so other lists stay unfiltered (D10): ListRPsBoundToWorkspace (api_workspace.go:1582; the WebUI intersects it with the filtered list), GetJobQueueStats, GetJobs/GetJobsV2, agents, and the admin-only dynamic pool list.

`det job list` without `-r` looks up the default compute pool in this list (job.py:24-25, :122-129) and fails with "Pool None not found" when that pool is hidden. Since D1 lets an admin restrict a global default, v1 changes check_is_priority to fail with "the default compute pool is not available to you; name a pool with -r" instead.

## 8. Admin API (Echo, like the dynamic-pool routes; no proto, no regenerated bindings)

  - New file core_resource_pool_access.go. m.registerResourcePoolAccessRoutes() is called next to m.registerDynamicResourcePoolRoutes() (core.go:1416).
  - Prefix /api/v1/resource-pool-access, not under /resource-pools/, so it cannot clash with gateway paths such as /resource-pools/{name}/workspace-bindings or a pool named "dynamic".
  - Auth: m.dynamicPoolAuth(update), unchanged (core_resource_pools.go:107). It authenticates the session itself (Echo routes bypass the gRPC interceptors), refuses inactive users, and uses CanGetMasterConfig to read and CanUpdateMasterConfig to write.
  - Body: decodeStrictBoundedJSON (core_resource_pools.go:384) cannot be reused, because validateDynamicPoolRequestJSON (:422) requires a "config" key. A small decoder: application/json only (415), http.MaxBytesReader at 64 KiB, DisallowUnknownFields, exactly one JSON value (400).
  - Any non-empty pool name is accepted, so an admin can restrict a pool before creating it.

Item, shared by every response:

```text
{"pool_name", "mode": "public"|"restricted", "exists": bool,
 "default_compute": bool, "default_aux": bool,
 "workspace_defaults": [{"workspace_id", "workspace", "kind": "compute"|"aux"}],
 "users": [{"id", "username", "active", "admin"}],
 "restricted_at", "restricted_by"}
```

  - mode is "restricted" exactly when a restriction row exists.
  - exists: the name is a pool of any RM in m.config.ResourceManagers() (with the automatic 'default' pool when resource_pools is omitted) or any dynamic_resource_pools row in any state. rm.GetResourcePools is not used, because it lists Ready pools only.
  - default_compute and default_aux: the pool is a global default of some RM in m.config (the fields checkIfRMDefaultsAreUnbound reads, core.go:1017-1065).
  - workspace_defaults: workspaces whose default_compute_pool or default_aux_pool is this pool.
  - users: the grants, shown even while the pool is public.

Routes:
  - `GET /api/v1/resource-pool-access`: 200 `{"resource_pools": [item...]}`, one item per name in the union of known pools, restriction rows and grant rows, sorted by name. A name with records but no pool is an orphan (exists=false).
  - `PUT /api/v1/resource-pool-access/:pool`, body `{"mode": "public"|"restricted"}`. "restricted" inserts the restriction row; "public" deletes it and keeps the grants (D17). Both are idempotent. Any pool may be restricted, global and workspace defaults included (D1, D11). 400 for any other mode or field.
  - `POST /api/v1/resource-pool-access/:pool/grant` and `.../revoke`, body `{"usernames": [...]}`. Stored whether the pool is public or restricted. Unknown usernames: 404 naming all of them, nothing changed (A1). Idempotent.
  - No delete route: set public and revoke express every state, including removing an orphan.
  - Every write returns 200 with the item and `warnings`, a list of strings:
      - when exists=false: `no resource pool named "P" exists; the setting applies to a pool created with this name`;
      - when the pool is restricted after the write, one per default it is: `"P" is the cluster's default compute pool: submissions that omit resources.resource_pool are refused for users without a grant on "P"` (likewise "default aux pool"; for a workspace: `"P" is the default compute pool of workspace "W": submissions there that omit ...`).
  - Each write logs at INFO, e.g. "resource pool access: admin restricted P", "made P public", "granted P to alice, bob", "revoked P from carol".

There is no startup warning. A master.yaml edit that points a default at a restricted pool shows in the Defaults column of `det rp access list`, and refused users see M1.

## 9. CLI (resource_pool.py)

Uses the raw session like create_dynamic; no bindings change. A new `access` group under `det resource-pool` (alias rp):

```text
det rp access list [--json]                       columns: Pool | Mode | Exists | Defaults | Users
det rp access set POOL [POOL ...] --mode {public,restricted}
det rp access grant POOL USERNAME [USERNAME ...]
det rp access revoke POOL USERNAME [USERNAME ...]
```

  - Every write prints the server's `warnings` to stderr, one line each. The CLI builds no warnings of its own.
  - Errors print the server message. `set` over several pools continues past a failure, prints one line per pool and exits 1 if any failed.
  - Defaults column: "cluster compute", "cluster aux", "W compute", "W aux".
  - `list --json` is also the export to save with each pg_dump.

## 10. WebUI

No code in v1 (D14). Server-side filtering limits every pool picker, and launch errors show the server message. Follow-ups: a "Manage access" action on ResourcePoolCard; ResourcepoolDetail.tsx:298 spins forever on a hidden pool (already so under RBAC); HyperparameterSearchModal falls back to resourcePools[0] when the experiment's pool is hidden (sent explicitly, so not a server re-route, but the form should require a choice).

## 11. Task identity: the D18 and D16 fixes

A task authenticates as its task spec's Owner, through DET_USER_TOKEN (pkg/tasks/task.go:204) or its allocation session (task/allocation.go:706, read back by grpcutil.GetUser at grpcutil/auth.go:149-162). Requests from inside a task are checked as that user. With the ACL, a non-admin's token carries that user's grants, which it did not carry before. Running another user's image or code under your session lends them your session: your grants, and for an admin every admin power, including the ACL routes (pre-existing). The two fixes below remove the cases where this happens without the user choosing it.

### D18 TensorBoard image inheritance (separate PR; prerequisite)
  - Today: LaunchTensorboard takes its task credentials from getCommandLaunchParams (api_tensorboard.go:231): the launcher's agent user group (api_command.go:82) and a session token minted for the launcher (api_command.go:161-174: getTaskSessionToken at :167, stored at :171). It then inherits from the most recent selected experiment (api_tensorboard.go:394-437): environment.image and the image pull secrets unless the request names its own image (:408-433), and registry_auth whenever the launch config (task container defaults, template or request) has none (:434-437, outside the custom-image block, so also when the request names its own image). tensorboard-entrypoint.sh runs `-m pip install` (unless DET_SKIP_PIP_INSTALL is set) and `-m determined.exec.tensorboard` with that image's Python.
  - In basic mode anyone may read any experiment and its artifacts (CanGetExperiment and CanGetExperimentArtifacts return nil, experiment/authz_basic_impl.go:18-30; checked at api_tensorboard.go:506-507, :524-525). So user A builds an experiment image; user B opens its TensorBoard; A's code runs with B's token and uid/gid. With the ACL it can submit into B's restricted pools, and if B is an admin, grant A access. B believes they are viewing metrics.
  - Fix: one ownership test gates both blocks: the image and pull-secret block (:408-433) and the separate registry_auth block (:434-437). The test passes only when the experiment it would inherit from (exp, loaded at api_tensorboard.go:397) has a non-nil OwnerID equal to the launcher's ID (user, :226). A non-owner, ordinary user or admin alike, inherits none of environment.image, the image pull secrets or registry_auth, whether or not the request names its own image. The TensorBoard keeps the image getCommandLaunchParams set (task container defaults at api_command.go:119, or the template's or request's image), its own pull secrets, and the launch config's registry_auth or none. An owner's launch is unchanged. The storage settings gathered from every selected experiment (api_tensorboard.go:280-392) are not code and are unchanged.
  - Prerequisite: yes. The ACL is what gives a non-admin token pool value, so the ACL deploys only after this fix is deployed.
  - Test: TestLaunchTensorboardInheritsImageOnlyFromOwnExperiment, each non-owner case run for an ordinary user and for an admin. A non-owner's launch gets the default image and no inherited pull secrets or registry_auth. A non-owner's launch that names its own image and no registry_auth keeps its image and gets no inherited registry_auth. An owner's launch still inherits all three, and with its own image still inherits registry_auth.

### D16 Continue keeps the owner's identity (separate PR; recommended, not a prerequisite)
  - Today: ContinueExperiment passes the request user as parseCreateExperiment's owner (api_experiment.go:1444, :1466-1469). parseCreateExperiment mints the task token for it and sets taskSpec.Owner (core_experiment.go:412-418) and dbExp.OwnerID (:428-431), from which newExperiment takes the agent user group (experiment.go:152). The DB owner does not change. So after an admin continue, the owner's code runs with the admin's token and uid/gid until a master restart, when restore rebuilds the owner from the DB (restore.go:102-106).
  - Fix: authorization (template access, project resolution, S3) checks the actor. The token, taskSpec.Owner, dbExp.OwnerID and the agent user group stay the experiment owner's (user.ByID(*origExperiment.OwnerID)). parseCreateExperiment gains an `actor` parameter.
  - Not a prerequisite: in basic mode only the owner or an admin may continue (CanEditExperiment, experiment/authz_basic_impl.go:68-79). The owner's continue keeps the owner's identity, and an admin token already carries every admin power, so the ACL adds nothing to it. Under RBAC, a non-owner with UPDATE_EXPERIMENT can continue another user's experiment; like fork and clone, that is a deliberate action on that experiment's code.
  - Test: an admin continues a non-admin's experiment; the trial's taskSpec.Owner and allocation session owner equal the experiment owner.

Deliberate paths stay as they are and the access doc names them: fork and clone (CreateExperiment with ParentId reuses the parent's model definition, core_experiment.go:393-404), generic fork or InheritContext children (api_generic_tasks.go:267-311), and a template that sets the image. When the deployed master does not include D16, continue by someone other than the owner (an admin; under RBAC, a holder of UPDATE_EXPERIMENT) is one more: it runs the owner's code with the continuer's token and uid/gid (D16, Today). If the ACL ships without D16, the access doc names this path as current behaviour, with no reference to D16, and the D16 PR removes that sentence from the access doc.

## 12. Rollout

  1. D18 PR (section 11). Merge and deploy.
  2. D16 PR (section 11), optional before the ACL; it can also follow it.
  3. The ACL PR. Deploy only after D18 is deployed. Its migration must be the newest at merge and at deploy (R1).

After deploy, existing pools stay public until an admin restricts them; the other changes at upgrade are listed in section 3 (a-f). Gap-free restrict: grant first, then restrict.

## 13. Error messages (gRPC code, and the HTTP status through the gateway)

  - M1 PermissionDenied (403): `user "alice" may not use resource pool "P": the pool is restricted; choose another pool or ask an administrator for access (if resources.resource_pool was not set, "P" is the default pool for this workspace or the cluster)`
  - M2 Unavailable (503): `could not check access to resource pool "P": <db error>; try again`
  - M3 InvalidArgument (400): `moving a job to another resource pool requires the target pool name`
  - M4 Internal (500): `resource pool access checked before the pool was resolved`
  - A1 Admin API 404: `unknown users: bob, carol; nothing was changed`. Also 400 for an invalid mode, an unknown field or more than one JSON value; 415 for a non-JSON content type; 401/403 from dynamicPoolAuth.

W1 returns M1. Bulk activate and ResumeRuns put M1 or M2 in the per-experiment result. The basic owner-or-admin refusal in PatchWorkspace keeps the existing PermissionDenied wrapping (api_workspace.go:724-727).

## 14. Docs and release note

  - New docs/maintenance/resource-pool-access.md, in the docs/maintenance/index.rst toctree, timeless. It covers: the rule; what is checked (S1-S7 and W1 in user terms) and what is not (section 6); revocation; restricting a global or workspace default, and its effect on submissions that omit a pool; who may change workspace defaults; new, adopted, renamed and deleted pools, and restricting a pool before creating it; grant, then restrict; the CLI; backup with `det rp access list --json`; and that running another user's image or code under your session lends that user your session (section 11), naming the deliberate paths.
  - docs/maintenance/dynamic-pools.md: one paragraph: new dynamic pools are public until restricted; restrict the name first to avoid a public window; adopt keeps access.
  - New docs/release-notes/resource-pool-access.rst in the existing :orphan: style: "New: per-pool access. Every resource pool is public unless an administrator restricts it; a restricted pool can be used only by administrators and the users granted access (`det rp access`). Access is checked when work is submitted, activated, unpaused, resumed or continued, when a job is moved to another pool, and when a workspace default pool is set. Pools a user cannot use are hidden from GET /api/v1/resource-pools. Upgrade: no pool changes who may use it until an administrator restricts it. Non-admin submissions, pool lists and workspace default changes now read the access tables, and fail with 503 when the database cannot be read; administrators are unaffected. Other changes: in basic auth mode, only a workspace's owner or an administrator can change its default pools; job-queue moves with an empty pool name are rejected; CreateExperiment with validate_only now also checks the resource pool set by an invariant config policy." (Matches section 3, deltas a-d.)
  - Outside this repo (follow-ups): the MCP server reports a hidden pool as "not present in the cluster inventory"; reword it to "not present or not available to you" and map a 403 to a permission error. The cluster's operations docs get a short "Resource pool access" section with the CLI.

## 15. Decisions

Settled:
  - Public by default: no record means public; a failed read is never public; restricted with no grants means admins only. Grants by user ID, records by pool name, two tables, no RBAC.
  - D1 Global default pools: admins may restrict them; the write response and the CLI warn, naming the pool (section 8).
  - D11 Workspace default pools: basic mode owner-or-admin, plus the pool-use check for every setter (W1). Restricting a pool that is already a workspace default is allowed and warned.
  - D12 New dynamic pools are public from creation; restrict the name first to avoid a window.
  - D13 A grant on a pool without a record is stored and dormant.
  - D15 Changing max_slots of an accepted experiment is management, not admission (section 6).
  - D16 Continue keeps the owner's identity: separate PR, not a prerequisite (section 11).
  - D18 TensorBoard inherits image, pull secrets and registry_auth only from the launcher's own experiment: separate PR, deployed before the ACL (sections 11 and 12).

Open; the design is written for the recommendation:
  - D3 Admin predicate: CanUpdateMasterConfig, the predicate that manages the ACL (users.admin in basic mode).
  - D4 The acting request user is checked.
  - D5 Checkpoint GC is a system task, exempt for every trigger.
  - D6 Work submitted from inside a running task after revocation is a new admission and is refused.
  - D7 A retried, persisted generic-task resume plan is a continuation.
  - D8 A job-queue move with an empty target is InvalidArgument.
  - D9 ValidateOnly and NTSC Preview report the denial.
  - D10 Other pool lists stay unfiltered.
  - D14 No WebUI code in v1.
  - D17 Grants survive "set public" and apply again on re-restrict. Alternative: delete them.

## Files

New:
  - `master/static/migrations/<ts>_add-resource-pool-access.tx.up.sql`
  - master/internal/poolaccess/poolaccess.go (CanUseResourcePool, UsablePools, the store, the package doc invariant) and poolaccess/poolaccess_intg_test.go
  - master/internal/core_resource_pool_access.go (routes, decoder, items, warnings) and core_resource_pool_access_intg_test.go
  - master/internal/api_command_pool_access_intg_test.go
  - docs/maintenance/resource-pool-access.md, docs/release-notes/resource-pool-access.rst

Changed:
  - master/internal/core.go (register the routes)
  - master/internal/api_command.go (S1), api_generic_tasks.go (S2, S6), core_experiment.go (S3)
  - master/internal/api_experiment.go (S3a at :1577 and :1683, S4, admit at :868, admitExperimentPool), api_runs.go (admit at :992)
  - master/internal/experiment/experiment_iface.go (ResourcePool() string), experiment/bulk_action.go (admit parameter), experiment/bulk_action_test.go
  - master/internal/api_job.go (S7), api_workspace.go (W1), workspace/authz_basic_impl.go (owner-or-admin), api_resourcepool.go (L1)
  - master/internal/checkpoint_gc.go (exemption comment at :101)
  - Tests: api_job_intg_test.go, api_workspace_intg_test.go, api_resourcepool_intg_test.go, api_experiment_intg_test.go, api_generic_intg_test.go, checkpoint_gc_test.go, restore_intg_test.go
  - harness/determined/cli/resource_pool.py (det rp access), job.py (hidden default pool message), harness/tests/cli/test_resource_pool.py
  - docs/maintenance/index.rst (toctree), docs/maintenance/dynamic-pools.md (one paragraph)

Not in this PR: api_tensorboard.go (D18) and the continue identity change (D16).

## Tests

Each test fails on the #35 branch without its change, except the regression pin at the end.

  - T1 poolaccess/poolaccess_intg_test.go TestPoolAccessStore. Both tables exist; pool_name '' violates the CHECK in both; a grant for a nonexistent user violates the FK. Restrict, MakePublic, Grant and Revoke are idempotent and report only what changed. Grant on a public pool succeeds; MakePublic keeps grants. restrictionsFor returns only restricted pools, with granted true only for the queried user.
  - T2 poolaccess/poolaccess_intg_test.go TestCanUseResourcePool (table). Non-admin: no record -> nil; restricted with no grants -> M1; restricted with a grant for u1 -> nil for u1, M1 for u2; a grant without a restriction -> nil; restricted then made public -> nil; pool '' -> Internal. Admin: restricted with no grants -> nil. With ReadRestrictions failing: a non-admin on a pool with no record gets Unavailable, never nil; an admin gets nil and the read is not called.
  - T3 api_resourcepool_intg_test.go TestGetResourcePoolsFiltersByAccess. Mock RM returns A, B, C; A public, B restricted with a grant to u1, C restricted with no grants. u1 sees [A, B]; u2 sees [A]; an admin sees [A, B, C]. u2 with limit=1 offset=1 gets [] and total 1. Unbound=true returns a subset of the filtered list. With a failing read, u2 gets an error.
  - T4 api_command_pool_access_intg_test.go TestLaunchNTSCChecksResolvedPool. The mock RM resolves '' to 'wsaux' for 0 slots and 'wscompute' otherwise, both restricted; 'open' is public. For a non-granted user: LaunchCommand without a pool is denied naming wscompute; LaunchTensorboard without a pool is denied naming wsaux; an explicit restricted pool and LaunchNotebook Preview are denied; LaunchCommand into 'open' succeeds. A granted user and an admin succeed. A denied launch adds no user_sessions row.
  - T5 api_generic_intg_test.go TestCreateGenericTaskChecksResolvedPool. For a non-granted user, denied: an omitted pool resolving to a restricted pool; slots 0 resolving to a restricted aux pool; a fork of an admin's task in a restricted pool; a child. No tasks or jobs row is added. A granted user's create and anyone's create into a public pool succeed.
  - T6 api_experiment_intg_test.go TestCreateExperimentChecksFinalPool. Denied, with no experiments row: an explicit restricted pool; a template that supplies one; an invariant policy that sets a restricted pool while the request names a public one; ValidateOnly. Allowed: PutExperiment (unmanaged) with a restricted pool; an admin; a public pool. For a granted user, the persisted resource_pool equals the checked pool. An invariant policy naming a pool not available to the workspace: CreateExperiment and ValidateOnly both return InvalidArgument (not Unknown) and add no row.
  - T7 api_experiment_intg_test.go TestCreateExperimentWritesBackCheckedPool. The mock RM returns a different pool for '' on its second call. A create that omits the pool persists the pool S3 checked, and newExperiment's resolution receives that explicit name. Second case: the global aux default is not Ready (resolved for '' with 0 slots, refused by explicit name): a zero-slot create that omits its pool fails and adds no row, while validate_only passes.
  - T8 api_experiment_intg_test.go TestContinueAndActivateCheckPool. A user creates and pauses an experiment in P; an admin restricts P with no grants. ActivateExperiment -> M1 and the state stays PAUSED; ActivateExperiments -> the denial in that ID's result; ResumeRuns -> reported; after the experiment ends, ContinueExperiment -> PermissionDenied through the api_experiment.go:1472 wrap, with state, config and restarts unchanged. An admin can activate and continue; after a grant, the user can.
  - T9 api_experiment_intg_test.go TestActivateOnCreateAndContinueReadsAccessOnce. ReadRestrictions succeeds on the first call and fails after. A granted non-admin's CreateExperiment with Activate into a restricted pool succeeds, the experiment is ACTIVE, and the count is 1; likewise ContinueExperiment. Without S3a the second read fails and leaves a PAUSED experiment behind Internal.
  - T10 experiment/bulk_action_test.go TestActivateExperimentsAdmit. With an admit that denies ID 2, experimentMock 2's ActivateExperiment is never called and its result carries the error; a nil admit returns an error. The existing TestActivateExperiments passes an allow-all admit.
  - T11 api_generic_intg_test.go TestUnpauseGenericTaskChecksPool. Unpausing a task whose pool was restricted after the pause -> M1 and no generic_task_resume rows; a member whose spec cannot be read fails the request; a pre-inserted pending plan still runs on retry; a granted user's task resumes.
  - T12 api_job_intg_test.go TestUpdateJobQueuePoolMoveChecksTarget. A move to a restricted target -> M1 and apply is not called for the batch; an empty target -> M3 and apply is not called; priority and weight updates for a job in a restricted pool apply; a move to a public pool passes preflight. The existing TestJobQueueUpdatesPreflightEveryOwner, including its admin move, passes unchanged.
  - T13 api_workspace_intg_test.go TestWorkspaceDefaultPools. Basic mode; W owned by non-admin o; R restricted with a grant to o and to g; P public. PatchWorkspace: a non-owner (g) setting W's default to P -> PermissionDenied and W unchanged; o setting R -> ok; the owner of W2 without a grant setting R -> M1 and W2 unchanged; an admin setting R on any workspace -> ok; after o's grant is revoked, o re-sending compute=R while setting aux to P -> ok; setting '' -> ok; with a failing read, o setting P -> Unavailable. PostWorkspace with default_compute_pool R: by a non-granted non-admin -> M1 and no workspace row; by g -> ok; by an admin -> ok.
  - T14 core_resource_pool_access_intg_test.go TestResourcePoolAccessRoutes, with the hooks of core_resource_pools_intg_test.go:34-45 and a real DB. Unauthenticated -> 401; a non-admin -> 403 on GET and writes. GET lists a known pool with no records as public with exists=true. PUT restricted on a name with no pool -> 200, exists=false and the exists warning; CanUseResourcePool then denies a non-admin on it. PUT restricted on a global default compute or aux pool -> 200 with the matching warning; on a workspace default -> 200 with that workspace in workspace_defaults and a warning naming it; PUT public -> no default warning. Grant on a public pool -> 200 with the user listed; grant, then restrict: the grantee is allowed throughout and another user is denied after the restrict. One unknown username -> 404 (A1) and nothing written. Revoke is idempotent; PUT public keeps grants; an orphan shows exists=false and PUT public removes its restriction. An unknown JSON field -> 400; a non-JSON content type -> 415.
  - T15 harness/tests/cli/test_resource_pool.py. det rp access list, set, grant and revoke send the expected requests and bodies; list renders Mode, Exists, Defaults and Users; every write prints each server warning; set over several pools continues past a failure and exits 1. `det job list` without -r, when no listed pool is the default compute pool, prints the hidden-default message.

Regression pin (passes before and after; fails only if a continuation is routed through the check): checkpoint_gc_test.go TestRunCheckpointGCTask and restore_intg_test.go restrict, with no grants, the pool each uses; GC still starts its allocation, and restore still restores the experiment and allocates its trial.

Run the touched master packages with -tags integration and -race, the harness CLI tests, and the lint for the changed Python files.

## Risks

  - R1 Migration order: if the ACL migration is not the newest at deploy, it never runs; every non-admin admission and pool list then fails with Unavailable. Fail-closed and visible; admins keep working (section 2).
  - R2 Losing the tables opens every pool: a pre-ACL pg_dump restore or a rollback to a pre-ACL binary (section 3).
  - R3 Rename and name reuse: a renamed pool is public; orphan records reattach to a new pool with an old name (section 3).
  - R4 Public window for a new dynamic pool unless its name was restricted first (section 3).
  - R5 Admission only: accepted work in a newly restricted pool keeps running and can still raise its max_slots; admins pause, kill or move it (section 6).
  - R6 Running another user's image or code under your session lends them your grants, or your admin powers. D18 must be deployed first; D16 and the deliberate paths remain (section 11).
  - R7 Pre-existing: grpcutil.GetUser's allocation-token path skips the Active check (grpcutil/auth.go:149-162), so a deactivated user's running task can still submit with that user's grants.
  - R8 Hiding: a user loses the cluster and queue view of a restricted pool where they still have work; the MCP server's wording needs its follow-up (section 14).
  - R9 No cache: during a database outage non-admins can neither list pools nor submit (section 4).
  - R10 S3 resolves after the invariant policies (section 3, deltas c and e). Container defaults and default priority still come from the pre-invariant pool (core_experiment.go:319-366), a pre-existing quirk left unchanged.
  - R11 Interface and signature changes (Experiment.ResourcePool, the admit parameter) break out-of-tree implementations and callers.
  - R12 A restricted global default refuses every non-granted submission that omits a pool; for the aux default that includes zero-slot TensorBoards, notebooks, shells, CPU commands and generic tasks. The write response warns (section 8).
  - R13 Workspace defaults: non-owners can no longer change them in basic mode (delta d), and a granted owner can make a restricted pool the default of a workspace others use (W1).

## History

Revisions 1-3 moved from "unset means admins only" to public by default, then closed review gaps (S3a, W1, the identity analysis). An external review of revision 3 led to this version: D18 and D16 as separate PRs with D18 deployed first, no max_slots admission check (S8 removed), admins may restrict global defaults with a warning, and workspace defaults guarded by owner-or-admin in basic mode plus the pool-use check. The third external review approved the design for implementation; after it, section 12 claims only that existing pools stay public, and D18 names the separate registry_auth block. The reasoning for each step is in the PR #37 discussion.
