# Pool ACL v1: per-user resource pool access (public by default)

Status: design for review, revision 3. The implementation follows in the same PR once the design is approved.

Base: stacked on PR #35 (feat/dynamic-pools-inherit, head 3ce0d87b9), because it relies on dynamic pools and adopt. It ships as its own PR after the 0.41.0 release (#36).

Line references. Every `file:line` reference is to commit 3ce0d87b9 (the head of #35). Every file the ACL touches outside the dynamic-pool code is identical at main commit dd3be1fce and at 3ce0d87b9, so one line number serves both. Files changed by #35 (core.go, core_resource_pools.go, rm/agentrm/*, db/postgres_dynamic_resource_pools.go, harness/determined/cli/resource_pool.py, docs/maintenance/dynamic-pools.md) hold only at 3ce0d87b9. #35 provides adopt (POST /api/v1/resource-pools/dynamic/:name/adopt), `resource_pools: []` for the agent RM, and the startup refusal when a default pool names no pool. agentrm ResolveResourcePool is at rm/agentrm/agent_resource_manager.go:515-573.

Paths. Paths that do not start with master/, harness/ or docs/ are relative to master/internal. Exceptions: agent_resource_manager.go and dynamic_pools.go (master/internal/rm/agentrm), jobservice.go (master/internal/job/jobservice), poolaccess_intg_test.go (master/internal/poolaccess), pkg/tasks/task.go (master/pkg/tasks), get_workspace.sql, patch_experiment.sql and tensorboard-entrypoint.sh (master/static/srv), migration-create.sh and migration-move-to-top.sh (master/static/migrations), ResourcepoolDetail.tsx (webui/react/src/pages/ResourcePool), go.mod (repository root), job.py and resource_pool.py (harness/determined/cli), and collection.go (go-pg/migrations v8.1.0 module). master.yaml means the master's configuration file, not a repository file.

Terms. "Admission" is the decision to accept new work into a pool. S1-S8 are the admission check sites, W1 is the workspace-default check and L1 is the list filter (section 5). D1-D18 are decisions for the repository owner (section 16). T1-T16 are tests and R1-R14 are risks (sections "Tests" and "Risks", after the entry-point table). "The MCP server" is the cluster's job-submission service for agents, a separate project that calls this master's REST API.

## 1. Rule

A user may start new work in pool P when any of these holds:
  - The user passes the admin predicate: cluster.AuthZProvider.Get().CanUpdateMasterConfig returns no permission error (basic mode: users.admin). The predicate is checked first and reads no ACL table, so admins keep working when the ACL tables cannot be read.
  - P has no restriction. A pool with no restriction record is public. This is the defined rule for every pool (existing, adopted, newly created, static or dynamic), not a fallback.
  - P is restricted and the user has a grant on P.

Everything else is denied:
  - restricted and no grant for this user, including restricted with no grants at all (admins only);
  - the access tables cannot be read (Unavailable; never treated as "no record");
  - an empty pool name (programming guard, Internal).

The check never picks another pool, and nothing re-routes a refused request.

## 2. Data model and migration

One migration, `master/static/migrations/<ts>_add-resource-pool-access.tx.up.sql`, created with migration-create.sh. `<ts>` must sort after 20261005000000 (the #35 spec migration) and must be the newest migration when the PR merges and when it is deployed: go-pg/migrations v8.1.0 skips every migration at or below the database's current version (collection.go:465). If #35 or main gains a later migration first, run migration-move-to-top.sh on this one. No down file: master/static/migrations/README.md says down migrations are not supported, and a rolled-back binary simply ignores the tables (section 3).

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

No seed, and no rows for public pools.
  - A restriction row's presence is what makes a pool restricted. "Set public" deletes the row. There is no mode column, so public cannot be stored in two ways.
  - Grants have no foreign key to the restriction row. A grant on a public pool is stored and has no effect until the pool is restricted. This gives the gap-free switch (grant first, then restrict) and lets an admin prepare a pool before it exists.
  - Grants survive "set public" and apply again if the pool is restricted again. Revoke removes them (owner decision D17).
  - Rows are keyed by pool name only: no foreign key to dynamic_resource_pools and no cluster_name. Pool names are globally unique (dynamic_resource_pools.pool_name UNIQUE; a dynamic pool may not take a static pool's name: ErrStaticResourcePoolConflict, declared at rm/agentrm/dynamic_pools.go:63 and raised on create at :169; the Echo create refuses a static name of another RM at core_resource_pools.go:156; the startup check at dynamic_pools.go:787-791 refuses a dynamic row that conflicts with a static pool), master.yaml pools have no dynamic row, adopt keeps the name, and rp_workspace_bindings is keyed the same way.
  - Access is never part of a dynamic pool's config or spec JSON. An access change never touches the pool registry, agents, spec_hash, revision, or a restart.
  - Grants are stored by user ID, because usernames can change; the API speaks usernames.
  - No optimistic concurrency. Each write is one idempotent statement, the last writer wins, and every write is logged at INFO with the admin and the change.

Code: one new package, master/internal/poolaccess, holding both the check (section 4) and the small store (bun, like db/postgres_rp_workspace_bindings.go). No new file in master/internal/db.
  - restrictionsFor(ctx, userID, pools []string) (map[string]bool, error). One query:

    ```sql
    SELECT r.pool_name, (g.user_id IS NOT NULL) AS granted
    FROM resource_pool_restrictions r
    LEFT JOIN resource_pool_grants g ON g.pool_name = r.pool_name AND g.user_id = ?
    WHERE r.pool_name IN (?)
    ```

    The map holds only restricted pools (value = granted). A pool absent from the map is public. A query error is returned as an error and never as an empty map. An empty pools slice returns an empty map without a query (bun cannot render an empty IN list, and GetResourcePools can legitimately have no pools to filter).
  - Restrict(ctx, pool, byUserID) (changed bool, err): INSERT ... ON CONFLICT DO NOTHING.
  - MakePublic(ctx, pool) (changed bool, err): DELETE ... WHERE pool_name = ?.
  - Grant(ctx, pool, userIDs, byUserID) (added []model.UserID, err): INSERT ... ON CONFLICT DO NOTHING RETURNING user_id.
  - Revoke(ctx, pool, userIDs) (removed []model.UserID, err): DELETE ... RETURNING user_id.
  - List(ctx) (restrictions, grants with username/active/admin, err): two SELECTs for the admin list.

## 3. Pool lifecycle under public by default

  - Upgrade: the migration creates two empty tables. No pool changes who may use it until an admin restricts it. Deploy order relative to the #35 conversion does not matter, and there is no post-upgrade step. What does change, for public pools too (a-d are in the release note; e and f are not user-visible regressions):
    - a. Every non-admin admission (S1-S8), every non-admin GetResourcePools (L1) and every non-admin change of a workspace default to a non-empty pool (W1) reads the access tables. A database error, or tables that are missing because the migration did not run (R1), fails those requests with Unavailable. Admins are unaffected.
    - b. A job-queue move with an empty target is rejected with InvalidArgument (S7, D8). Before this change, jobservice.go:205-209 only logs it and setRP("") moves the job to a default pool (experiment.go:981-997).
    - c. CreateExperiment with validate_only now resolves the pool an invariant config policy sets (S3). A policy that names a pool that is not Ready or not available to the workspace now fails validation with InvalidArgument; before, validation passed (CheckExperimentConstraints does not look at the pool, configpolicy/task_config_policy.go:78-101). The real create of such a request already failed before saving (newExperiment, Internal); it now fails at S3 with InvalidArgument.
    - d. PatchExperiment: a request refused for its max_slots change (the max_slots authz check or S8) no longer applies its name, notes, description or label change (S8 preflight).
    - e. A zero-slot experiment that omits its pool while the global aux default is not Ready now fails before it is saved; before, it was saved and then failed to start, leaving its row (S3, side effect of the write-back). ValidateOnly still passes it.
    - f. CreateExperimentResponse.Config carries the resolved resource_pool.
  - New dynamic pools (POST /api/v1/resource-pools/dynamic): public from the moment they are Ready. Create is unchanged (no access field; create replay and spec_hash are untouched). To avoid any public window for a pool meant to be restricted, restrict the name before creating the pool: `det rp access set P --mode restricted` works on a name that has no pool yet (section 8). Restricting while the pool is still Pending also leaves no window, as long as the restriction lands before the pool becomes Ready: a named submission into a Pending pool fails, because ResolveResourcePool only accepts names in the ready list (agent_resource_manager.go:546-567).
  - Adopt: never reads or writes access records. Access follows the name, so an adopted pool keeps its restriction and grants.
  - Delete and re-create, rollback of a conversion, re-adopt: records are never deleted automatically, so a pool that vanishes and comes back under the same name gets its restriction and grants back.
  - Rename of a master.yaml pool: the new name is a new, public pool. The old name's records stay as orphans, listed with exists=false. Restrict the new name, and clean the orphans with set public and revoke.
  - Rolling back to a binary without the ACL: the tables stay and are ignored, so every pool is public, as before the ACL. Rolling forward again does not re-run the migration, so the restrictions return as they were.
  - Restoring a pg_dump taken before the ACL: the migration runs again and creates empty tables, so every pool is public. Save `det rp access list --json` next to each pg_dump and reapply from it.

## 4. The central check

Package master/internal/poolaccess. It imports db, cluster, model and grpc status. It applies in basic and RBAC mode alike. It does not replace FilterResourcePools, workspace bindings or the RBAC checks; those stay as additional checks.

```go
// CanUseResourcePool returns nil when user may start new work in pool, PermissionDenied when
// not, and Unavailable or Internal when access could not be decided. pool must be the final,
// resolved name.
func CanUseResourcePool(ctx context.Context, user model.User, pool string) error
```

CanUseResourcePool:
  1. pool == "": codes.Internal "resource pool access checked before the pool was resolved".
  2. cluster.AuthZProvider.Get().CanUpdateMasterConfig(ctx, &user): err -> codes.Internal; no permission error -> nil (admin).
  3. `m, err := ReadRestrictions(ctx, user.ID, []string{pool})`, where `var ReadRestrictions = restrictionsFor` is exported and documented as a test hook, so tests in master/internal can make the read fail or count reads, as core_resource_pools.go:25-37 does for its request-user and authorize hooks. err -> codes.Unavailable (message 2). This return comes before any look at m, so a failed read can never reach the public branch.
  4. `granted, restricted := m[pool]`; !restricted -> nil (public by rule); granted -> nil; otherwise log one INFO line (user, pool) and return PermissionDenied (message 1).

```go
// UsablePools returns, for each name, whether user may use it (admins: all true, no read).
// One query, no cache; an error is returned, never a partial answer.
func UsablePools(ctx context.Context, user model.User, pools []string) (map[string]bool, error)

// CanSetWorkspaceDefaultPool returns nil when user may make pool a workspace default:
// admins always, everyone else only while the pool is public. Not an admission check.
func CanSetWorkspaceDefaultPool(ctx context.Context, user model.User, pool string) error
```

CanSetWorkspaceDefaultPool:
  1. pool == "": codes.Internal (callers never check "", which unsets the default).
  2. Admin predicate as in CanUseResourcePool: no permission error -> nil, no read.
  3. ReadRestrictions(ctx, user.ID, []string{pool}): err -> codes.Unavailable (message 2).
  4. pool present in the map (restricted, whether or not this user holds a grant) -> PermissionDenied (message 5). Otherwise nil (public).

  - The check takes model.User by value, so there is no nil-user path, and no "no user = allowed" branch exists anywhere.
  - No cache: each call reads the database, so a restriction or revocation applies from the next request.
  - Linearization point: an admission is decided when CanUseResourcePool reads the tables. A restriction or revocation committed after that read does not stop that request; this is inherent to admission-only semantics.
  - Callers pass the acting request user from grpcutil.GetUser(ctx) (D4). In basic mode a non-admin can only act on their own jobs, so for non-admins the actor is the owner; admins bypass.
  - Identity of code inside a task: a task authenticates as its task spec's Owner, through DET_USER_TOKEN (pkg/tasks/task.go:204) or its allocation session (task/allocation.go:706, read back by grpcutil.GetUser at grpcutil/auth.go:149-162). That Owner is the user of the request that launched or continued the work (core_experiment.go:412/:418, api_command.go:167, api_generic_tasks.go:169), which is not always the job's DB owner: on an admin continue it is the admin (section 15, gap 1). Requests made from inside a task are checked as that identity. The code inside the task is not always the identity's own: a TensorBoard on another user's experiment runs that experiment's image with the launcher's token and uid/gid (D18). Running someone else's image or code under your session lends them your grants.

Package doc invariant:
  - CanUseResourcePool is called only from S1-S8 (section 5); CanSetWorkspaceDefaultPool only from W1 (PatchWorkspace and PostWorkspace).
  - Nothing at or below task.DefaultService.StartAllocation or rm.Allocate calls either function.
  - Checkpoint GC and every restore, restart and resume continuation are exempt by design (section 6).

## 5. Check sites

All at the user-operation layer, after the final pool is known, before anything is persisted.

### S1 getCommandLaunchParams (api_command.go:61)
  - Where: after :145-146 set config.Resources.ResourcePool and Slots from the resolved poolName; before CheckNTSCConstraints (:162) and getTaskSessionToken (:167, which writes a user_sessions row). CanUseResourcePool(ctx, *userModel, poolName.String()), userModel from :74.
  - Nothing changes the NTSC pool after :145 (NTSC invariant overrides are still a TODO at :153, and the template cannot override the pool because :145 overwrites it).
  - Covers LaunchCommand (:342, call at :350), LaunchShell (api_shell.go:210, call at :218), LaunchNotebook (api_notebook.go:267, call at :275) including Preview (:308), and LaunchTensorboard (api_tensorboard.go:215, call at :231; MustZeroSlot, so an omitted pool resolves to the aux default). Zero-slot launches are checked like any other.

### S2 getGenericTaskLaunchParameters (api_generic_tasks.go:74)
  - Where: after :138-139 set RawResourcePool from the resolved poolName; before validateGenericTaskScheduling (:152) and getTaskSessionToken (:169). CanUseResourcePool(ctx, *userModel, poolName.String()).
  - Covers CreateGenericTask (:243) as a plain create, a fork (ForkedFrom :267; the forked config is resolved again) and a child (ParentId, InheritContext :302), before the persist transaction (comment at :323, db.Bun().RunInTx at :327).

### S3 parseCreateExperiment (core_experiment.go:277)
  - Where: immediately after `config = *configWithInvariantOverrides` (:380), only when !req.GetUnmanaged(), before getTaskSessionToken (:412):

    ```go
    if owner == nil { return codes.Internal }   // never allowed
    res := config.Resources()
    final, err := m.rm.ResolveResourcePool(rm.ResourcePoolName(res.ResourcePool()), workspaceID, res.SlotsPerTrial())
    if err != nil { return status.Errorf(codes.InvalidArgument, "invalid resource configuration: %s", err) }
    if err := poolaccess.CanUseResourcePool(ctx, *owner, final.String()); err != nil { return err }
    res.SetResourcePool(final.String()); config.SetResources(res)   // write-back
    ```

  - Error code: m.rm.ResolveResourcePool returns plain errors, so the failure is wrapped as InvalidArgument explicitly, the code the first resolution at :319 gets through m.ResolveResources (spec_util.go:38-41). errors.Wrapf would surface as Unknown. A DB error inside ResolveResourcePool (GetDefaultPoolsForWorkspace) gets InvalidArgument here as it does upstream at :319.
  - The user checked is the parameter named `owner`, which both managed callers set to the request user, the actor (api_experiment.go:1466-1469, :1622-1624). If the separate identity fix (D16) adds an `actor` parameter, S3 must use `actor`.
  - Why after :380: an invariant config policy can set resources.resource_pool (configpolicy/task_config_policy.go:182-215 merges the policy over the user's config), so the pool resolved at :319 is not final.
  - The write-back is load-bearing. newExperiment resolves again (experiment.go:109). With the resolved name written back, that resolution is of an explicit name, which returns the same name or fails (agent_resource_manager.go:546-572). So the checked pool is the pool AddExperiment persists (experiment.go:146), with no window for a workspace default change in between.
  - Side effect of the write-back: with the global aux default not Ready, a zero-slot experiment that omits its pool now fails in newExperiment before it is saved. newExperiment resolves the written-back explicit name, which the ready list refuses (experiment.go:109-114); CreateExperiment returns Internal "failed to create experiment" (api_experiment.go:1673-1674) and AddExperiment (experiment.go:146) never runs. Before, the omitted name took the early default branch (agent_resource_manager.go:524-532, no ready check), ValidateResources returned early for 0 slots (:632-634), AddExperiment saved the row, and e.Start() then failed (start -> setWeight -> rm.SetGroupWeight -> poolByName, Ready pools only: experiment.go:257, agent_resource_manager.go:599-604, :741-749) with "failed to start experiment N" (api_experiment.go:1678-1680), leaving the row. So this is an earlier failure, not a new one. S3's own resolution of the omitted name still takes the early branch and succeeds, and ValidateOnly returns at api_experiment.go:1655 before newExperiment, so ValidateOnly still passes this case. Experiments with slots_per_trial > 0 already failed at :319 (ValidateResources -> CheckMaxSlotsExceeded -> poolByName). CreateExperimentResponse.Config now carries the resolved resource_pool, which is harmless.
  - ValidateOnly behaviour change: S3 resolves the post-invariant pool, so ValidateOnly now also fails (InvalidArgument) when an invariant config policy names a pool that is not Ready or not available to the workspace. Before, ValidateOnly resolved only the pre-invariant pool at :319 and CheckExperimentConstraints does not look at the pool (configpolicy/task_config_policy.go:78-101); the real create already failed in newExperiment, before saving.
  - Covers CreateExperiment (api_experiment.go:1592): create; fork and clone (ParentId); templates (merged at :293-300); invariant config policies; ValidateOnly (:1655), which now reports the denial; Activate.
  - Covers ContinueExperiment (:1441, calling it at :1466). newExperiment with ID != 0 never persists, and the continue transaction (:1482-1567) runs after S3, so a denial leaves nothing behind.
  - PutExperiment (:1739) is unmanaged-only and skips S3, like every unmanaged path; none allocates. An unmanaged experiment becomes managed only through ContinueExperiment, which reaches S3.
  - grpc-go v1.64.1 (go.mod:54) status.FromError unwraps errors, so ContinueExperiment's fmt.Errorf wrap at :1472 still yields PermissionDenied or Unavailable.

### S3a Create or continue with activation performs no second check (gap 3)
  - CreateExperiment (:1683) and ContinueExperiment (:1577) call a.ActivateExperiment, the RPC handler. Replace both with e.ActivateExperiment(), where e is the *internalExperiment returned by newExperiment (:1672, :1477) and registered by e.Start() (experiment.go:218-233).
  - The re-authorization the handler did is already done in the same request on the same experiment: CanEditExperiment at :1667 (create with Activate) and at :1449 (continue). This holds under RBAC too: ExperimentAuthZRBAC.CanEditExperiment (experiment/authz_rbac.go:278-294) checks UPDATE_EXPERIMENT on the experiment's workspace, which is the same workspace in both checks.
  - Effect: one admission decision per request, at S3's read. A revocation or a read failure between S3 and activation can no longer leave a persisted PAUSED experiment (create) or a reset, started, PAUSED experiment (continue) behind an Internal error.

### S4 apiServer.ActivateExperiment (api_experiment.go:847), the external RPC only
  - Where: after the registry Load (:855), before e.ActivateExperiment() (:859): admitExperimentPool(ctx, e).
  - admitExperimentPool is a package-level function in master/internal: grpcutil.GetUser(ctx), then CanUseResourcePool(ctx, *user, e.ResourcePool()).
  - Add `ResourcePool() string` to experiment.Experiment (experiment/experiment_iface.go:48-60). internalExperiment already implements it (experiment_job_service.go:86) from its live active config, which reflects job-queue moves. experimentMock embeds the interface, so it still compiles.

### S5 experiment.ActivateExperiments (experiment/bulk_action.go:215)
  - Add a required parameter `admit func(context.Context, Experiment) error`, called for each ref before ref.ActivateExperiment() (:244). A non-nil error becomes that experiment's ExperimentActionResult.Error and the experiment is not activated. A nil admit returns an error; it never means "allow".
  - Callers pass admitExperimentPool: apiServer.ActivateExperiments (api_experiment.go:868) and pauseResumeAction (api_runs.go:992), which serves ResumeRuns (:899). pauseResumeAction is package-level, so it passes the package-level function directly; nothing is threaded through it.
  - The callback keeps poolaccess and cluster out of the experiment package.

### S6 UnpauseGenericTask (api_generic_tasks.go:838)
  - Where: in the `len(plan) == 0` branch, after the state checks and before makeGenericTaskResumePlan (:908, which persists the plan).
  - For each member of tasksToResume in state Paused (the members makeGenericTaskResumePlan takes), read getGenericTaskSpec (:977) and check spec.GenericTaskConfig.Resources.ResourcePool() for the actor. That is the pool the resume allocates in (generic_task_resume.go:378).
  - Any denial returns before anything is written. A read error or a nil spec fails the request; a member is never skipped.
  - Retrying a plan that was already persisted (len(plan) > 0, :877-893) is a continuation and is not checked again (D7).
  - Not inside runGenericTaskResume: recoverGenericTaskResumes (generic_task_resume.go:420-439) runs it at startup with no user.
  - Resume reuses each member's stored spec, including its Owner and token (generic_task_resume.go:303, :372), so an admin unpause does not change the task's identity.
  - makeGenericTaskResumePlan keeps its signature (6 test call sites).

### S7 updateJobQueueAuthorized (api_job.go:137)
  - Where: in the per-update preflight loop, after authorize (:146), for QueueControl_ResourcePool:
      - target == "": codes.InvalidArgument (message 3). Before this change, jobservice.go:205-209 only logs this and setRP resolves "" to a default pool (experiment.go:989), which is an unchecked re-route.
      - otherwise poolaccess.CanUseResourcePool(ctx, curUser, target), called directly; the function keeps its signature.
  - Any failure rejects the whole batch before apply.
  - An explicit target resolves to itself in setRP (experiment.go:989-997), so the checked name is the name used.
  - Priority and weight updates are never pool-checked. Commands and generic tasks already refuse pool moves (command/command_job_service.go:98, generic_task_job.go:244).
  - The existing test's admin case (model.User{Admin: true} moving a job to "other") still passes, because the admin bypass reads no table.

### S8 PatchExperiment raising max_slots (api_experiment.go:987) (gap 2, D15)
  - Where: a preflight block right after CanEditExperimentsMetadata (:1003-1006) and before `madeChanges := false` (:1008), so before any write (the metadata write is at :1067-1068). Only when req.Experiment.Resources != nil && Resources.MaxSlots != nil:

    ```go
    // moved here from :1087-1090, unchanged, so authz still answers first
    if err = experiment.AuthZProvider.Get().CanSetExperimentsMaxSlots(ctx, *curUser, modelExp, newMax); err != nil {
        return nil, echo.NewHTTPError(http.StatusForbidden, err.Error())
    }
    pre, err := a.m.db.ActiveExperimentConfig(int(exp.Id))   // read only for this decision
    if err != nil { return nil, errors.Wrapf(err, "unable to load config for experiment %v", exp.Id) }
    if cur := pre.Resources().MaxSlots(); cur != nil && newMax > *cur {
        if err := poolaccess.CanUseResourcePool(ctx, *curUser, pre.Resources().ResourcePool()); err != nil { return nil, err }
    }
    ```

  - A raise is a new value greater than a non-nil current max_slots. A nil current means unlimited, so no new value is a raise. Lowering or keeping the value is never checked.
  - The pool is the stored active config's. setRP saves a moved pool with SaveExperimentConfig (experiment.go:1002), so it agrees with the live pool.
  - The existing load at :1076 stays where it is and is not replaced by the preflight read. patch_experiment (master/static/srv/patch_experiment.sql: `SET config = config || $2, notes = $3`) merges name, labels and description into the experiments.config column, and SaveExperimentConfig (:1200) writes the whole column from the config loaded at :1076. A config read before the metadata write and saved after it would undo the metadata change, so the preflight read is used only for the decision. This is why the preflight uses a second read rather than moving the :1076 load before the metadata write.
  - Effect: a request refused for its max_slots change (authz or S8) writes nothing; a combined name and max_slots request is all or nothing for these two checks. Moving CanSetExperimentsMaxSlots is a small upstream ordering change (section 3, delta d). configpolicy.CanSetMaxSlots (:1180-1183, InvalidArgument) keeps its place.
  - Linearization point: the preflight read. A pool move committed between that read and :1076 is itself an admitted move and is not checked again.
  - Why max_slots and not priority or weight: max_slots is the ceiling of slots an accepted experiment may use at once, fixed when it was admitted. Raising it is new resource use beyond what was admitted. Priority and weight only reorder or reshare demand that was already admitted, within that ceiling.

### W1 Workspace default pools: only admins make a restricted pool a workspace default (gap 4, D11)
  - Rule: a non-admin may set a workspace's default compute or aux pool only to a public pool. A restricted pool becomes a workspace default only when an admin sets it, whether or not the setter holds a grant. Check: poolaccess.CanSetWorkspaceDefaultPool (section 4).
  - Why not "a pool the setter can use" (the rule of revision 2): basic mode lets any user patch any workspace's defaults (CanSetWorkspacesDefaultPools returns nil, workspace/authz_basic_impl.go:187-191; getWorkspaceAndCheckCanDoActions, api_workspace.go:279-298, through GetWorkspaceByID, refuses only immutable or archived workspaces). A granted non-admin could then point another user's or a shared workspace at the restricted pool, and every non-granted submission there that omits a pool would be refused. Nothing warns anyone when the default is set: the admin API's workspace_defaults is read only when an admin looks.
  - PatchWorkspace (api_workspace.go:676): inside the existing default-pools block, after each availability check (:743-746 compute, :751-754 aux), when the new value is non-empty and differs from the current one (currWorkspace.DefaultComputePool / DefaultAuxPool, filled by get_workspace.sql through GetWorkspaceByID at :256): CanSetWorkspaceDefaultPool(ctx, currUser, value). Re-sending the current value is not a change and is not checked, so a client that echoes an admin-set restricted default while changing the other field is not refused. "" (unset) is never checked.
  - PostWorkspace (api_workspace.go:473): after the existing authz and validation (:481-501), before the model.Workspace literal (:503-506, defaults stored at :505) and the insert transaction (:510), for each non-empty req.DefaultComputePool / req.DefaultAuxPool: the same check. Without it, `det workspace create W --default-compute-pool R` would bypass the rule. PostWorkspace's missing availability check is upstream and stays as it is.
  - W1 is not an admission check: a grant does not change its answer.

### L1 GetResourcePools list filter (api_resourcepool.go:45)

Not an admission check. See section 7.

## 6. Exempt internal paths (continuations and system tasks; never call the check)

  - Experiment restore: restore.go:60-128 (resolves again at :78, owner from the DB at :102-106, newExperiment with ID != 0 at :128). Startup: core.go:1423.
  - Trial allocations: trial.go:438 (restored allocation) and :502 (new allocation), including searcher-created trials (experiment.go:655-662, reused by continueTrials) and restored trials (restore.go:197), the two newTrial call sites; restarts after failure or preemption; PatchRP after an admitted move (trial.go:250-255).
  - Command, shell, notebook and TensorBoard restore: command/command.go:161, from RestoreAllCommands (core.go:1443).
  - Generic task restore: core.go:879-960, StartAllocation at :938 (startup :1448).
  - Generic task resume: recoverGenericTaskResumes (generic_task_resume.go:420-439, startup core.go:1451) and a retried persisted plan (S6).
  - Checkpoint GC: checkpoint_gc.go:101 calls ResolveResourcePool("", -1, 0), which always gives the global aux default; StartAllocation at :175. Exempt for every trigger: experiment end (experiment.go:462-475), experiment delete, a checkpoint-storage PatchExperiment, DeleteTensorboardFiles, CheckpointsRemoveFiles, DeleteCheckpoints. Add a comment at :101: "System task: exempt from the resource pool ACL by design; see the poolaccess package doc." The global aux default is public under D1 in any case.

## 7. List filtering

GetResourcePools (api_resourcepool.go:45):
  - Between FilterResourcePools (:70-73) and the sort (:75), drop every pool for which UsablePools is false.
  - The Unbound subset (:78-84, built from filteredPools) and Paginate (:86) then see only usable pools, so the totals are right.
  - On a read error, return the error, never the unfiltered list. Admins see every pool (no read).
  - Under public by default, only restricted pools the user has no grant on disappear.

Effect: every reader of this endpoint shows only usable pools: the WebUI launch forms (NtscLaunchModal intersects with it), HyperparameterSearchModal, ManageJob "move to pool", the cluster pages, the MCP server's inventory, the SDK and the CLI.

Left unfiltered (pool names are not secret): ListRPsBoundToWorkspace (api_workspace.go:1582; the WebUI intersects it with the filtered list), GetJobQueueStats, GetJobs/GetJobsV2, agents, and the admin-only dynamic pool list.

## 8. Admin API (Echo, like the dynamic-pool routes; no proto, no regenerated bindings)

  - New file master/internal/core_resource_pool_access.go. m.registerResourcePoolAccessRoutes() is called in core.go next to m.registerDynamicResourcePoolRoutes() (core.go:1416).
  - Prefix /api/v1/resource-pool-access, not under /resource-pools/, so it cannot clash with gateway paths such as /resource-pools/{name}/workspace-bindings or with a pool named "dynamic".
  - Auth: reuse m.dynamicPoolAuth(update) unchanged (core_resource_pools.go:107): it authenticates the session itself (Echo /api/v1 routes bypass the gRPC interceptors), refuses inactive users, and uses CanGetMasterConfig for reads and CanUpdateMasterConfig for writes (admin-only in basic mode).
  - Body decoding: decodeStrictBoundedJSON (core_resource_pools.go:384) cannot be reused because validateDynamicPoolRequestJSON (:422) requires a "config" key. Add a small decoder: application/json only (415 otherwise), http.MaxBytesReader at 64 KiB, DisallowUnknownFields, exactly one JSON value (400 otherwise).
  - Pool names in paths: any non-empty name is accepted. Writes on a name with no pool are allowed and reported with exists=false, so an admin can restrict a pool before creating it, and the CLI can warn about typos.

Item (shared by every response):

```text
{"pool_name", "mode": "public"|"restricted", "exists": bool,
 "default_compute": bool, "default_aux": bool,
 "workspace_defaults": [{"workspace_id", "workspace", "kind": "compute"|"aux"}],
 "users": [{"id", "username", "active", "admin"}],
 "restricted_at", "restricted_by"}
```

  - mode is "restricted" exactly when a restriction row exists, and "public" otherwise.
  - exists: the name is a known pool, meaning a pool of any RM in m.config.ResourceManagers() (with the automatic 'default' pool when resource_pools is omitted) or any dynamic_resource_pools row in any state (m.db.ListDynamicResourcePools(ctx, "")). rm.GetResourcePools is not used, because it lists Ready pools only.
  - default_compute and default_aux: the pool is the default of some RM in m.config (the fields checkIfRMDefaultsAreUnbound reads, core.go:1017-1050).
  - workspace_defaults: workspaces whose default_compute_pool or default_aux_pool is this pool (one query on workspaces).
  - users: the grants, shown even while the pool is public, where they have no effect.

Routes:

  - `GET /api/v1/resource-pool-access`: 200 `{"resource_pools": [item...]}`, one item per name in the union of known pools, restriction rows and grant rows, sorted by name. A known pool with no records is listed as public with no users. A name with records but no pool is an orphan (exists=false).
  - `PUT /api/v1/resource-pool-access/:pool`, body `{"mode": "public"|"restricted"}`: 200 with the item.
      - "restricted": inserts the restriction row (idempotent). 409 when the pool is a global default compute or aux pool (D1, message A1). A workspace default is allowed and reported in workspace_defaults (D11).
      - "public": deletes the restriction row if there is one (idempotent; on an orphan this is the cleanup path). Grants are kept (D17).
      - 400 for any other mode or field.
  - `POST /api/v1/resource-pool-access/:pool/grant`, body `{"usernames": [...]}`: 200 with the item. Stored whether the pool is public or restricted. 404 naming every unknown username, all or nothing (message A2). Idempotent.
  - `POST /api/v1/resource-pool-access/:pool/revoke`, body `{"usernames": [...]}`: 200 with the item. 404 naming every unknown username, all or nothing. Idempotent for users who have no grant.
  - No delete route: set public and revoke express every state, including removing an orphan.
  - Each write logs at INFO, e.g. "resource pool access: admin restricted P", "made P public", "granted P to alice, bob", "revoked P from carol".

## 9. CLI (harness/determined/cli/resource_pool.py)

Uses the raw session like create_dynamic; no bindings change. A new `access` group under `det resource-pool` (alias rp):

```text
det rp access list [--json]                       columns: Pool | Mode | Exists | Defaults | Users
det rp access set POOL [POOL ...] --mode {public,restricted}
det rp access grant POOL USERNAME [USERNAME ...]
det rp access revoke POOL USERNAME [USERNAME ...]
```

  - Errors print the server message. `set` over several pools continues past a failure, prints one line per pool (ok or the error) and exits 1 if any failed.
  - After a write, the CLI prints a warning when exists=false ("no resource pool named P exists; the setting applies to a pool created with this name"), and, for restricted, one line per workspace default ("P is the default `<kind>` pool of workspace W: submissions there that omit resources.resource_pool are refused for users without a grant").
  - Defaults column: "cluster compute", "cluster aux", "W compute", "W aux".
  - `list --json` is also the export to save next to pg_dump.

## 10. WebUI

No code in v1 (D14). Server-side filtering already limits every pool picker; admins see all pools; launch errors show the server message. Follow-ups: a "Manage access" action on ResourcePoolCard; ResourcepoolDetail.tsx:298 spins forever on a hidden pool (already so under RBAC); HyperparameterSearchModal falls back to resourcePools[0] when the experiment's pool is hidden (visible in the form and sent explicitly, so not a server re-route, but the form should require a choice).

## 11. Revocation semantics

Admission only.
  - Restricting a pool affects S1-S8 and W1 from the next request on; revoking a grant affects S1-S8 (W1 ignores grants). Nothing is killed, paused or moved.
  - These continue unchecked: running and queued allocations; accepted experiments and their searcher-created trials; trial restarts; restores of experiments, commands and generic tasks after a master restart; persisted generic-task resume plans; checkpoint GC.
  - Logs, kill, pause, and priority or weight edits keep their existing permissions. Lowering max_slots is always allowed.
  - A user without access to a restricted pool cannot activate, unpause, continue, fork or clone into it, cannot move a job into it, and cannot raise an experiment's max_slots there. No non-admin, granted or not, can make a restricted pool a workspace default (W1); a default an admin set stays.
  - Work submitted later from inside a running task with that user's token is a new admission and is refused (D6).
  - Admins who want running work gone kill it explicitly.
  - The pool disappears from that user's pool lists, and with it its cluster and queue page. The user's own experiment and task pages still work.
  - Linearization point: section 4.

## 12. Error messages (gRPC code, and the HTTP status through the gateway)

  1. PermissionDenied (403): `user "alice" may not use resource pool "P": the pool is restricted; choose another pool or ask an administrator for access (if resources.resource_pool was not set, "P" is the default pool for this workspace or the cluster)`
  2. Unavailable (503): `could not check access to resource pool "P": <db error>; try again`
  3. InvalidArgument (400): `moving a job to another resource pool requires the target pool name`
  4. Internal (500): `resource pool access checked before the pool was resolved` (programming guard)
  5. PermissionDenied (403): `only an administrator can make restricted resource pool "P" a workspace default: submissions in this workspace that omit resources.resource_pool would be refused for users without a grant` (W1)

Bulk activate and ResumeRuns put message 1 or 2 in the per-experiment result.

Admin API:
  - A1. 409 `resource pool "P" is the default <compute|aux> pool, so it must stay public: submissions that omit resources.resource_pool use it; point default_<compute|aux>_resource_pool at another pool in master.yaml and restart first`
  - A2. 404 `unknown users: bob, carol; nothing was changed`
  - 400 for an invalid mode, an unknown field or more than one JSON value; 415 for a non-JSON content type; 401/403 from dynamicPoolAuth.

## 13. Docs and release note

  - New docs/maintenance/resource-pool-access.md, in the docs/maintenance/index.rst toctree. Timeless. It covers: the rule (public by default, restricted, grants, admin bypass); what is checked (S1-S8 and W1 in user terms) and what is not (continuations, checkpoint GC); revocation semantics; that global default pools stay public, and how to restrict the pool that is the aux or compute default (move the default in master.yaml, restart, then restrict); workspace defaults; how new, adopted, renamed and deleted pools behave, and restricting a pool before creating it; the gap-free restrict procedure (grant, then restrict); that only admins make a restricted pool a workspace default; the CLI; backup with `det rp access list --json`; that running another user's image or code under your session lends that user your session: your grants, and for an admin every admin power including the ACL itself. Name the unexpected path (opening a TensorBoard on another user's experiment runs that experiment's image with your token and uid/gid, until D18 lands) and the knowing ones (admin continue until D16 lands; fork, clone, generic fork or child; a template that sets the image).
  - docs/maintenance/dynamic-pools.md: one paragraph: new dynamic pools are public until restricted; restrict the name first to avoid any public window; adopt keeps access.
  - New docs/release-notes/resource-pool-access.rst in the existing :orphan: style: "New: per-pool access. Every resource pool is public unless an administrator restricts it; a restricted pool can be used only by administrators and the users granted access (`det rp access`). Access is checked when work is submitted, activated, unpaused, resumed or continued, when a job is moved to another pool, and when an experiment's max_slots is raised. Only administrators can make a restricted pool a workspace default. Pools a user cannot use are hidden from GET /api/v1/resource-pools. Upgrade: no pool changes who may use it until an administrator restricts it. Non-admin submissions, pool lists and workspace default changes now read the access tables, and fail with 503 when the database cannot be read; administrators are unaffected. Other changes: job-queue moves with an empty pool name are rejected; CreateExperiment with validate_only now also checks the resource pool set by an invariant config policy; a PatchExperiment request refused for its max_slots change no longer applies its name, notes, description or label changes." (It matches section 3, deltas a-d.)
  - Outside this repo (follow-ups): the MCP server's admission check reports a hidden pool as "not present in the cluster inventory"; reword it to "not present or not available to you" and map a 403 to a permission error. The cluster's own operations docs get a short, timeless "Resource pool access" section with the CLI.

## 14. Scope kept small

Not in v1, on purpose: startup warnings (a restricted global default can only come from a master.yaml edit, and `det rp access list` shows defaults and orphans); the `det job list` fix in job.py (under D1 the default compute pool is always visible; `-r` with a hidden pool reports "not found"); WebUI code; filtering of the other pool lists; any access field on dynamic pool create; a delete route.

## 15. Gaps found in review: verification and resolution

### Gap 1: actor vs owner for admin continue, fork, clone and child
  - Verified. ContinueExperiment passes the request user as parseCreateExperiment's owner (api_experiment.go:1444, :1466-1469), and CanEditExperiment lets an admin do this (experiment/authz_basic_impl.go:70-79). parseCreateExperiment mints the task session for that user (core_experiment.go:412), sets taskSpec.Owner (:418) and dbExp.OwnerID/Username (:428-431). newExperiment takes the agent uid/gid from dbExp.OwnerID (experiment.go:152). Trials clone that spec (experiment.go:655, :662; trial.go:541). The container gets the admin's DET_USER_TOKEN and allocation session. The DB owner does not change (the continue transaction updates state and config only, :1482-1567). Restore rebuilds the owner from the DB (restore.go:102-106).
  - Resolution for the ACL: S3 keeps checking the actor (D4). On an admin continue, fork, clone or generic fork/child, the admin's decision admits the work, which is what the admin bypass means. Section 4 states which identity a container authenticates as.
  - The token and uid/gid identity is not an ACL property. For an admin token the ACL adds nothing: the leaked credential is a full admin session, which also authenticates the Echo admin routes (user.Service.UserAndSessionFromRequest -> ByToken), so code holding it could already do anything, now including rewriting the ACL. For a non-admin token the ACL does add something: before it, another non-admin's token carried no pool privilege; with it, the token carries that user's grants. The same holds for every path that runs one user's image or code under another user's session, including the unexpected TensorBoard path (gap 5, D18).
  - Fix it in a separate PR (D16): ContinueExperiment loads the experiment's owner (user.ByID(*origExperiment.OwnerID)) and uses it for the token, taskSpec.Owner, dbExp.OwnerID and through that the agent group; parseCreateExperiment gains an `actor` parameter for template access, project resolution and S3. Test: an admin continues a non-admin's experiment; the trial's taskSpec.Owner and allocation session owner equal the experiment owner. If that PR lands first, S3 uses `actor`.
  - Admin fork, clone (CreateExperiment with ParentId reuses the parent's model definition, core_experiment.go:393-404) and generic fork or InheritContext children (api_generic_tasks.go:267-311) create admin-owned jobs by design. The access doc says so.

### Gap 2: max_slots raises after revocation
  - Verified. PatchExperiment sets max_slots (api_experiment.go:1086-1092) and applies it to the running experiment (:1173-1184); in basic mode this is owner-only (experiment/authz_basic_impl.go:103-107); `det e set max-slots` reaches it. No check site covered it before S8.
  - Resolution: S8 treats a raise above the current non-nil max_slots as admission (D15). Lowering is never checked. Priority and weight stay unchecked (reason in S8).

### Gap 3: double check on create/continue with activate leaving partial state
  - Verified. CreateExperiment persists and starts the experiment (:1672, :1678) and then calls the handler (:1683); ContinueExperiment commits its transaction (:1482-1567), starts (:1573) and calls the handler (:1577). Both rewrap any activation error as codes.Internal (:1685, :1579).
  - Resolution: S3a. The internal callers call e.ActivateExperiment() directly; S4 runs only on the external RPC. One admission decision per request, at S3's read; no partial state from the ACL; the denial keeps its own code because it comes from S3. The MCP server, which always creates with activate:true, gets PermissionDenied or Unavailable before anything exists.

### Gap 4: workspace default pools being restricted
  - Verified. Workspace defaults are read by GetDefaultPoolsForWorkspace (db/postgres_rp_workspace_bindings.go:275) and win over the global defaults in ResolveResourcePool (agent_resource_manager.go:519-544). PatchWorkspace changes them after only CanSetWorkspacesDefaultPools (basic: nil) and an availability check (api_workspace.go:722-757). In revision 1, D1's 409 and the startup warning covered global defaults only.
  - Resolution, admin side: restricting a pool that is a workspace default is allowed, because a group workspace whose default is that group's restricted pool is a legitimate setup. It is never silent: every item carries workspace_defaults, the PUT response lists them, and the CLI prints a warning per workspace. Refused submissions name the pool and say it may be the workspace default (message 1).
  - Resolution, user side: W1. A non-admin may make only a public pool a workspace default, in PatchWorkspace and in PostWorkspace; a grant does not change that. Revision 2's rule, "a pool the setter can use", still let any granted non-admin break a shared workspace's defaults (see W1 and Revisions).
  - Global defaults keep D1's 409, the same shape as upstream's refusal to bind a default pool to a workspace (checkIfPoolIsDefault, api_resourcepool.go:214-234).

### Gap 5: TensorBoard runs another user's image under the launcher's session (found in the revision 3 review)
  - Verified. LaunchTensorboard takes the token and uid/gid of the launcher through getCommandLaunchParams (api_tensorboard.go:231; api_command.go:74, GetAgentUserGroup :82, getTaskSessionToken :167, DET_USER_TOKEN at pkg/tasks/task.go:204). Unless the request names its own image (model.UsingCustomImage, api_tensorboard.go:408), it copies environment.image and the image pull secrets of the most recent selected experiment (:396, :408-431) and inherits its registry_auth (:435-436). tensorboard-entrypoint.sh runs `$DET_PYTHON_EXECUTABLE -m pip install ...` and `-m determined.exec.tensorboard` from that image. In basic mode anyone may open a TensorBoard on any experiment (CanGetExperimentArtifacts returns nil, experiment/authz_basic_impl.go:26-30; checks at api_tensorboard.go:507, :525). The other values gathered from all selected experiments (:280-380) are storage settings only (S3 keys and endpoint, the shared_fs host path), not code.
  - Consequence: user A crafts an experiment image; granted user B opens its TensorBoard; A's code holds B's token and can submit into a restricted pool R as B (S1-S3 check B and allow it). If B is an admin, it can also grant A access through the admin API. B believes they are only viewing metrics.
  - Resolution: D18, a separate small PR that lands before the ACL is deployed (the ACL is what gives a non-admin token pool value). LaunchTensorboard inherits environment.image, the image pull secrets and registry_auth only from an experiment the launcher owns: the experiment whose image would be inherited, exps[len(exps)-1] (:396), must have OwnerID equal to the launcher's ID. Otherwise the TensorBoard keeps the task-container-default image from getCommandLaunchParams and no inherited registry_auth or pull secrets; a request that names its own image keeps it. No admin exception, so admin tokens are protected too. Fork, clone, generic fork or child, and a template that sets the image are knowing actions; the access doc names them (section 13).

## 16. Owner decisions

Each decision lists the recommended option first. Every decision in 16.1 and 16.2 is open for the owner; the design is written for the recommended option. 16.3 lists what is already settled.

### 16.1 Open, with alternatives

  - **D1. Global default pools.** Open. Recommended: they stay public. PUT restricted on a global default compute or aux pool returns 409 (message A1). Why: a restricted global default would refuse every non-granted submission that omits a pool, cluster-wide. The cluster's aux default pool receives every zero-slot TensorBoard, notebook, shell, CPU command and zero-slot generic task that omits a pool. Upstream refuses to bind a default pool to a workspace for the same reason. There is no startup warning: a master.yaml edit that points a default at a restricted pool is visible in `det rp access list` and in message 1. Alternative: allow it with a warning.
  - **D11. Workspace default pools.** Open. Recommended: only admins make a restricted pool a workspace default, in PatchWorkspace and PostWorkspace (W1). Restricting a pool that is already a workspace default is allowed and always reported. Alternatives: (i) also let the workspace owner who holds a grant set it (owner-or-admin, like CanSetWorkspacesName), which still lets a group lead point a workspace others use at the group pool; (ii) revision 2's weaker "the setter can use the pool", which lets any granted non-admin point any mutable workspace at it; (iii) 409 on restricting a pool that is a workspace default (blocks group workspaces); (iv) no W1.
  - **D15. max_slots raise is admission (gap 2).** Open. Recommended: yes (S8). A raise above the current non-nil value is checked. Alternative: treat max_slots like priority and weight, and document that admins cap a revoked user by killing or moving the work.
  - **D16. Continue keeps the owner's identity (gap 1).** Open. Recommended: fix it in a separate small PR, independent of the ACL. S3 checks the actor either way.
  - **D17. Grants survive "set public".** Open. Recommended: keep them, dormant (fewer writes; restricting again restores the list; `list` shows them). Alternative: delete them on set public.
  - **D18. TensorBoard on another user's experiment (gap 5).** Open. Recommended (a): a separate small PR, landed before the ACL is deployed. LaunchTensorboard inherits image, pull secrets and registry_auth only from an experiment the launcher owns, and otherwise keeps the default image. Alternatives: (b) mint the TensorBoard's token and uid/gid for the owner of the experiment whose image is inherited, as D16 does for continue (more change: the task then belongs to someone other than the launcher); (c) document only.

### 16.2 Open, recommendation awaiting confirmation

  - **D3. Admin bypass predicate.** Open. Recommended: CanUpdateMasterConfig, the same predicate that manages the ACL. It is identical to users.admin in basic mode.
  - **D4. Which user is checked.** Open. Recommended: the acting request user. Gap 1 shows that the identity inside a task is a separate, pre-existing issue (D16).
  - **D5. Checkpoint GC.** Open. Recommended: a system task, exempt for every trigger. It cannot choose its pool and runs zero-slot in the global aux default, which D1 keeps public.
  - **D6. Work submitted from inside a running task after revocation.** Open. Recommended: a new admission, refused. Under public by default this only matters for restricted pools.
  - **D7. Retry of a persisted generic-task resume plan.** Open. Recommended: a continuation, not checked again.
  - **D8. Job-queue move with an empty target.** Open. Recommended: InvalidArgument. setRP("") resolves to a default pool, which may be a restricted workspace default, so accepting it would be an unchecked re-route.
  - **D9. ValidateOnly and NTSC Preview report the denial.** Open. Recommended: yes; it follows from the check placement.
  - **D10. Other pool lists stay unfiltered.** Open. Recommended: yes. Pool names are not secret, and the WebUI forms intersect with the filtered GetResourcePools.
  - **D14. WebUI.** Open. Recommended: no WebUI code in v1. Under public by default nobody loses a pool from their lists until an admin restricts it.

### 16.3 Settled

  - Public by default (owner decision): no record means public; a failed read is never public; restricted with no grants means admins only.
  - **D12. New dynamic pools**, settled by that decision: public from creation, like every pool without a record. Restrict the name before create to avoid a public window. Create is unchanged.
  - **D13. Grant on a pool without a record**, settled by that decision: allowed and stored, with no effect while the pool is public. This is what makes grant-then-restrict and preparing a pool before it exists work. Unknown usernames still return 404.
  - D2 (deploy order) no longer exists: nothing is seeded and every pool stays public until an admin restricts one, so any deploy order is behaviour-neutral and there is no post-upgrade step.
  - Release: the ACL ships as its own PR after 0.41.0.

## Entry points (entry point -> exact check location)

| Entry point | Check |
|---|---|
| LaunchCommand, api_command.go:342 (call :350) | S1: getCommandLaunchParams, after :145-146 (pool and slots set from the resolved poolName), before CheckNTSCConstraints :162 and getTaskSessionToken :167 |
| LaunchShell, api_shell.go:210 (call :218) | S1, same location |
| LaunchNotebook, api_notebook.go:267 (call :275), including Preview :308 | S1, same location; Preview reports the denial |
| LaunchTensorboard, api_tensorboard.go:215 (call :231), zero-slot | S1, same location; an omitted pool is the resolved aux default |
| CreateGenericTask, api_generic_tasks.go:243: plain, fork (ForkedFrom :267), child (ParentId, InheritContext :302) | S2: getGenericTaskLaunchParameters, after :138-139 (RawResourcePool set), before validateGenericTaskScheduling :152 and getTaskSessionToken :169; before the persist transaction (RunInTx :327) |
| CreateExperiment, api_experiment.go:1592: create, fork/clone (ParentId), template (:293-300), invariant policy, ValidateOnly (:1655) | S3: parseCreateExperiment, right after `config = *configWithInvariantOverrides` core_experiment.go:380, managed only, before getTaskSessionToken :412; resolve with m.rm.ResolveResourcePool (failure -> InvalidArgument), check, write the name back |
| CreateExperiment with Activate | S3 only; :1683 calls e.ActivateExperiment() instead of a.ActivateExperiment (S3a) |
| ContinueExperiment, api_experiment.go:1441 | S3 (call :1466), before newExperiment :1477 and the transaction :1482; :1577 calls e.ActivateExperiment() (S3a) |
| ActivateExperiment RPC, api_experiment.go:847 | S4: after ExperimentRegistry.Load :855, before e.ActivateExperiment() :859: admitExperimentPool(ctx, e) |
| ActivateExperiments, api_experiment.go:865 (call :868) | S5: experiment/bulk_action.go:244, admit(ctx, ref) before ref.ActivateExperiment(); denial goes into that ID's result |
| ResumeRuns, api_runs.go:899 -> pauseResumeAction :908 | S5: admitExperimentPool passed at api_runs.go:992 |
| UnpauseGenericTask, api_generic_tasks.go:838 | S6: in the len(plan) == 0 branch, before makeGenericTaskResumePlan :908, every Paused member's stored spec pool; a persisted plan (:877-893) is not checked |
| UpdateJobQueue, api_job.go:112 -> updateJobQueueAuthorized :137 | S7: preflight loop after authorize :146, QueueControl_ResourcePool only: "" -> InvalidArgument, else CanUseResourcePool(target); whole batch rejected before apply |
| PatchExperiment max_slots, api_experiment.go:987 | S8: preflight after CanEditExperimentsMetadata :1003-1006, before `madeChanges := false` :1008 and the metadata write :1067-1068: CanSetExperimentsMaxSlots (moved from :1087-1090), a separate read of the active config, and CanUseResourcePool on its pool when the current max_slots is non-nil and the new value is greater; the load at :1076 stays |
| PatchWorkspace default pools, api_workspace.go:676 | W1: in the default-pools block, after the availability checks :743-746 (compute) and :751-754 (aux), for a non-empty value that differs from the current one: CanSetWorkspaceDefaultPool(currUser, value) (admins, or a public pool) |
| PostWorkspace default pools, api_workspace.go:473 | W1: after the authz and validation :481-501, before the model.Workspace literal :503-506 and the insert transaction :510, for each non-empty req.DefaultComputePool / DefaultAuxPool: CanSetWorkspaceDefaultPool(curUser, value) |
| GetResourcePools, api_resourcepool.go:45 (list filter, not admission) | L1: between FilterResourcePools :70-73 and the sort :75, keep pools UsablePools marks true; read error -> error |
| Admin routes GET/PUT /api/v1/resource-pool-access[/:pool[/grant/revoke]] | Registered next to registerDynamicResourcePoolRoutes, core.go:1416; dynamicPoolAuth (core_resource_pools.go:107): CanGetMasterConfig to read, CanUpdateMasterConfig to write |
| Exempt (never checked) | restore.go:60-128 and restored trials restore.go:197; searcher-created trials experiment.go:655-662; trial.go:438, :502, :250-255; command/command.go:161; core.go:879-960 (:938); generic_task_resume.go:420-439 and a persisted plan; checkpoint_gc.go:101/:175 |

## Files

New:
  - `master/static/migrations/<ts>_add-resource-pool-access.tx.up.sql` (`<ts>` after 20261005000000, newest at merge and deploy)
  - master/internal/poolaccess/poolaccess.go (CanUseResourcePool, UsablePools, CanSetWorkspaceDefaultPool, store, package doc with the invariant)
  - master/internal/poolaccess/poolaccess_intg_test.go
  - master/internal/core_resource_pool_access.go (Echo routes, decoder, item building)
  - master/internal/core_resource_pool_access_intg_test.go
  - master/internal/api_command_pool_access_intg_test.go
  - docs/maintenance/resource-pool-access.md
  - docs/release-notes/resource-pool-access.rst

Changed:
  - master/internal/core.go (register the routes)
  - master/internal/api_command.go (S1)
  - master/internal/api_generic_tasks.go (S2, S6)
  - master/internal/core_experiment.go (S3)
  - master/internal/api_experiment.go (S3a at :1577 and :1683, S4, admit at :868, S8 preflight with CanSetExperimentsMaxSlots moved into it, admitExperimentPool)
  - master/internal/api_runs.go (admit at :992)
  - master/internal/experiment/experiment_iface.go (ResourcePool() string)
  - master/internal/experiment/bulk_action.go (admit parameter)
  - master/internal/experiment/bulk_action_test.go
  - master/internal/api_job.go (S7)
  - master/internal/api_job_intg_test.go
  - master/internal/api_workspace.go (W1 in PatchWorkspace and PostWorkspace)
  - master/internal/api_workspace_intg_test.go
  - master/internal/api_resourcepool.go (L1)
  - master/internal/api_resourcepool_intg_test.go
  - master/internal/api_experiment_intg_test.go
  - master/internal/api_generic_intg_test.go
  - master/internal/checkpoint_gc.go (exemption comment at :101)
  - master/internal/checkpoint_gc_test.go, master/internal/restore_intg_test.go (regression pins)
  - harness/determined/cli/resource_pool.py (det rp access list/set/grant/revoke)
  - harness/tests/cli/test_resource_pool.py
  - docs/maintenance/index.rst (toctree), docs/maintenance/dynamic-pools.md (one paragraph)

Not in this PR: api_tensorboard.go (D18 is its own PR) and the continue-identity change (D16).

## Tests

Each test below fails on the #35 branch without its change. The last item is a regression pin and is listed separately.

  - T1 poolaccess_intg_test.go TestPoolAccessStore. Both tables exist; pool_name '' violates the CHECK in both; a grant for a nonexistent user ID violates the FK. Restrict, MakePublic, Grant and Revoke are idempotent and report only what changed. Grant on a name with no restriction succeeds. MakePublic keeps the grants. restrictionsFor returns only restricted pools, with granted true only for the queried user. Fails on #35: the package and the tables do not exist.
  - T2 poolaccess_intg_test.go TestCanUseResourcePool (table). Non-admin: no record -> nil (public by rule); restricted with no grants -> PermissionDenied with message 1; restricted with a grant for u1 -> nil for u1, PermissionDenied for u2; a grant with no restriction -> nil; restricted then MakePublic -> nil; pool '' -> Internal. Admin: restricted with no grants -> nil. With ReadRestrictions replaced by a failing read: a non-admin on a pool with NO record gets Unavailable, never nil (proves a failed read is not "no record"); a non-admin on a restricted pool gets Unavailable; an admin gets nil and the read is not called. Fails on #35: missing. Same file, TestCanSetWorkspaceDefaultPool: non-admin on a public pool -> nil; non-admin on a restricted pool -> PermissionDenied (message 5) with and without a grant; admin -> nil without a read; failing read for a non-admin -> Unavailable; '' -> Internal.
  - T3 api_resourcepool_intg_test.go TestGetResourcePoolsFiltersByAccess. Mock RM returns A, B, C; A has no record, B is restricted with a grant to u1, C is restricted with no grants. u1 sees [A, B]; u2 sees [A]; an admin sees [A, B, C]. u2 with limit=1 offset=1 gets [] and total 1. Unbound=true returns a subset of the filtered list. With a failing read, u2 gets an error, not the list. Fails on #35: no filter.
  - T4 api_command_pool_access_intg_test.go TestLaunchNTSCChecksResolvedPool. Mock RM resolves '' to 'wsaux' for 0 slots and to 'wscompute' otherwise (as workspace defaults would); both restricted; 'open' has no record. For a non-granted user: LaunchCommand without a pool is denied naming wscompute; LaunchTensorboard without a pool is denied naming wsaux; an explicit restricted pool is denied; LaunchNotebook Preview is denied; LaunchCommand into 'open' succeeds. A granted user and an admin succeed. A denied launch adds no user_sessions row. Fails on #35: no check.
  - T5 api_generic_intg_test.go TestCreateGenericTaskChecksResolvedPool. For a non-granted user, PermissionDenied on: an omitted pool resolving to a restricted pool; slots 0 resolving to a restricted aux pool; a fork of an admin's task in a restricted pool; a child (ParentId). No tasks or jobs row is added. A granted user's create and anyone's create into a pool with no record succeed. Fails on #35.
  - T6 api_experiment_intg_test.go TestCreateExperimentChecksFinalPool. Denied: an explicit restricted pool; a template that supplies one; an invariant config policy that sets resources.resource_pool to a restricted pool while the request names a public one (proves the check runs after core_experiment.go:380); ValidateOnly. No experiments row is added. Allowed: PutExperiment (unmanaged) with a restricted pool; an admin; a pool with no record. For a granted user, the persisted config's resource_pool equals the checked pool. An invariant policy naming a pool not available to the workspace: CreateExperiment and ValidateOnly both return InvalidArgument (not Unknown), and no row is added. Fails on #35: no check, and ValidateOnly passes the unavailable policy pool.
  - T7 api_experiment_intg_test.go TestCreateExperimentWritesBackCheckedPool. The mock RM records ResolveResourcePool calls and returns a different pool for '' on its second call (a workspace default changed between the two resolutions). A create that omits the pool persists the pool S3 checked, and newExperiment's resolution receives that explicit name, not ''. Second case: the global aux default is not Ready (the mock resolves '' with 0 slots to it but refuses it by explicit name, as agentrm does): a zero-slot create that omits its pool fails and adds no experiments row, while the same request with validate_only passes. Fails without the write-back (the second resolution gets '', the persisted pool can differ from the checked one, and the not-Ready case leaves a row).
  - T8 api_experiment_intg_test.go TestContinueAndActivateCheckPool. A user creates and pauses an experiment in P (no record); an admin restricts P with no grants. ActivateExperiment -> PermissionDenied, state stays PAUSED; ActivateExperiments -> the denial in that ID's result; ResumeRuns -> reported; after the experiment ends, ContinueExperiment -> PermissionDenied (the code survives the :1472 wrap) and the row's state, config and restarts are unchanged. An admin can activate and continue. After a grant, the user can. Fails on #35.
  - T9 api_experiment_intg_test.go TestActivateOnCreateAndContinueReadsAccessOnce (gap 3). ReadRestrictions is replaced by a counter that succeeds on the first call and fails on every later call. A granted non-admin's CreateExperiment with Activate into a restricted pool succeeds, the experiment is ACTIVE, and the count is 1; the same holds for ContinueExperiment. Fails without S3a: the second read inside a.ActivateExperiment fails, the request returns Internal, and the experiment is left PAUSED.
  - T10 experiment/bulk_action_test.go TestActivateExperimentsAdmit. With an admit that denies ID 2, experimentMock 2's ActivateExperiment is never called (the mock asserts it) and its result carries the error; a nil admit returns an error. The existing TestActivateExperiments passes an allow-all admit. Fails on #35: no admit parameter.
  - T11 api_generic_intg_test.go TestUnpauseGenericTaskChecksPool. Unpausing a paused task whose pool was restricted after the pause -> PermissionDenied and no generic_task_resume rows; a member whose stored spec cannot be read fails the request; a pre-inserted pending plan for the root still runs on retry; a granted user's task resumes. Fails on #35.
  - T12 api_job_intg_test.go TestUpdateJobQueuePoolMoveChecksTarget. With a real restriction row: a move to the restricted target -> PermissionDenied and apply is not called for the batch; an empty target -> InvalidArgument and apply is not called; priority and weight updates for a job whose current pool is restricted are applied; a move to a pool with no record passes preflight. The existing TestJobQueueUpdatesPreflightEveryOwner is unchanged and passes. Fails on #35: the restricted and empty targets pass preflight.
  - T13 api_experiment_intg_test.go TestPatchExperimentMaxSlotsRaiseChecksPool. An experiment with max_slots 1 in P; an admin then restricts P with no grants. The owner's PATCH to 4 -> PermissionDenied, the stored active config keeps 1 and SetGroupMaxSlots is not applied; the owner's PATCH {name: "x", resources.max_slots: 4} -> PermissionDenied and the name is unchanged (preflight runs before the metadata write); PATCH to 1 and to 0 succeed; an admin's PATCH to 4 succeeds; for an experiment with no max_slots, the owner's PATCH to 64 succeeds. Combined allowed request: a granted owner's PATCH {name: "y", resources.max_slots: 2} stores both, and the stored config's name is "y" (guards against reusing a config read before the metadata write, which SaveExperimentConfig at :1200 would write back over the patched name). Fails on #35: the raise is not checked and the denied combined request keeps the name.
  - T14 api_workspace_intg_test.go TestWorkspaceDefaultPoolNeedsAdminWhenRestricted. R is restricted with a grant to g and available to workspace W owned by o (a non-admin). PatchWorkspace setting W's compute or aux default to R: a non-granted user -> PermissionDenied (message 5) and W unchanged; g (granted, not the owner) -> PermissionDenied and W unchanged; o holding a grant -> PermissionDenied; an admin -> ok. After the admin sets R as W's compute default, a non-admin PATCH that re-sends compute=R and sets aux to a public pool -> ok. Setting '' and setting a public pool -> ok. With a failing read, a non-admin setting a public pool -> Unavailable. PostWorkspace by a non-admin with default_compute_pool R -> PermissionDenied and no workspace row; by an admin -> ok; by a non-admin with a public default -> ok. Fails on #35: no check in either handler.
  - T15 core_resource_pool_access_intg_test.go TestResourcePoolAccessRoutes, with the request-user and authorize hooks of core_resource_pools_intg_test.go:34-45 and a real DB. Unauthenticated -> 401; a non-admin -> 403 on GET and on writes. GET lists a known pool with no records as public with exists=true. PUT restricted on a name with no pool -> 200 with exists=false, and CanUseResourcePool then denies a non-admin on that name (restrict before create). PUT restricted on a global default compute or aux pool -> 409 (A1); PUT public on it -> 200. PUT restricted on a workspace's default pool -> 200 with that workspace in workspace_defaults. Grant on a public pool -> 200 with the user listed. Grant, then restrict: the grantee is allowed at every step and another user is denied after the restrict. A grant with one unknown username -> 404 (A2) and nothing written. Revoke is idempotent. PUT public keeps the grants. An orphan shows exists=false and PUT public removes its restriction. An unknown JSON field -> 400; a non-JSON content type -> 415. Fails on #35: no routes.
  - T16 harness/tests/cli/test_resource_pool.py. det rp access list, set, grant and revoke send GET, PUT and POST to /api/v1/resource-pool-access[/P[/grant|/revoke]] with the expected bodies; list renders Mode, Exists, Defaults and Users; set over several pools continues past a 409, prints the server message and exits 1; the CLI prints the exists=false and workspace-default warnings. Fails on #35: the commands do not exist.

Tests in the separate PRs: D18, TestLaunchTensorboardInheritsImageOnlyFromOwnExperiment (a non-owner's launch, admin included, gets the default image and no inherited registry_auth or pull secrets; an owner's launch still inherits; a request with its own image keeps it). D16, as in section 15, gap 1.

Regression pin (passes before and after; fails only if a continuation is ever routed through the check): checkpoint_gc_test.go TestRunCheckpointGCTask and restore_intg_test.go insert a restriction with no grants for the pool each uses; GC still starts its allocation, and restore still restores the experiment and allocates its trial.

Run the touched master packages with -tags integration and -race, the harness CLI tests, and the lint for resource_pool.py.

## Risks

  - R1 Migration order: go-pg v8.1.0 skips any migration at or below the database's version (collection.go:465). If the ACL migration is not the newest at deploy time it never runs; the tables are then missing and every non-admin admission and pool list fails with Unavailable (fail-closed and immediately visible; admins keep working). Run migration-move-to-top.sh if another migration lands first.
  - R2 Losing the tables' contents opens pools: a pre-ACL pg_dump restore, or a rollback to a pre-ACL binary, makes every pool public. Save `det rp access list --json` next to each pg_dump and reapply from it.
  - R3 Rename opens: renaming a restricted master.yaml pool yields a public pool under the new name, with no startup warning; the old name shows as exists=false in `det rp access list`.
  - R4 Name reuse: orphan restrictions reattach to a new pool with an old name (fails closed), and orphan grants reattach too (opens that pool to the old grantees if it is restricted). Clean orphans with set public and revoke.
  - R5 Public window for new pools: a dynamic pool is public once Ready unless its name was restricted first.
  - R6 Admission only: queued experiments, searcher-created trials, restarts, restores and persisted resume plans in a newly restricted pool keep running; admins kill them explicitly.
  - R7 Running another user's image or code under your session lends them your session. With the ACL a non-admin session carries that user's grants (new); an admin session carries every admin power, including the ACL (pre-existing). Unexpected path: a TensorBoard on another user's experiment runs its image with the launcher's token and uid/gid until D18 lands. Knowing paths: admin continue (the admin's token and uid/gid until a master restart; D16 fixes it), fork, clone, generic fork or child, a template that sets the image.
  - R8 Pre-existing: grpcutil.GetUser's allocation-token path skips the Active check (grpcutil/auth.go:149-162), so a deactivated owner's running task can still submit; the ACL still applies that user's grants.
  - R9 Hiding: a user loses the cluster and queue view of a restricted pool where they still have work; the MCP server reports a hidden pool as "not present in the cluster inventory" until its follow-up (section 13).
  - R10 No cache: every non-admin pool list and admission does one indexed read; during a database outage non-admins can neither list pools nor submit, and admins keep working.
  - R11 S3 resolution after the invariant policies: ValidateOnly now fails for a policy pool that is not Ready or not available (before, only the real create failed), and a zero-slot create that omits its pool while the global aux default is not Ready fails before saving instead of after (ValidateOnly still passes that case). Container defaults and default priority still come from the pre-invariant pool (core_experiment.go:319-366), a pre-existing quirk left unchanged.
  - R12 Interface and signature changes (Experiment.ResourcePool, ActivateExperiments' admit) break out-of-tree implementations and callers; the in-tree ones are in the file list.
  - R13 D1 means restricting the pool that is the cluster's aux default first needs a master.yaml default change and a restart. A master.yaml edit can still point a default at a restricted pool; non-granted submissions that omit a pool are then refused with message 1.
  - R14 W1 friction: a workspace owner who holds a grant cannot make the group's restricted pool their workspace default; an admin sets it (D11 alternative (i) if that is too strict). PatchWorkspace (for a changed non-empty default) and PostWorkspace (for a non-empty default) now read the access tables for non-admins.

## Revisions

  - Revision 1 (first draft). A pool without a record was "unset" and usable by admins only. The migration seeded records, so the draft needed deploy sequences, an owner decision on deploy order (D2), an amendment to the dynamic-pool conversion procedure, startup warnings for restricted default pools, and a `det job list` fix in job.py. A grant on a pool without a record returned 409.
  - Revision 2. The owner decided that pools are public by default. The rule became "no record means public"; a failed read stays Unavailable, and restricted with no grants stays admins only. The data model became two tables with no mode column and no foreign key from grants to restrictions, so grants on public pools are stored and dormant. There is no seed and no down file, and the store moved into the poolaccess package with the ReadRestrictions test hook. The deploy sequences, D2, the procedure amendment, the startup warnings and the job.py change were dropped. The admin API accepts any non-empty name, keeps grants on set public and reports workspace defaults. A review of revision 1 found four gaps (section 15, gaps 1-4), which added S3a, S8 (D15), W1 (D11), and the corrected statement of task identity with D16 as a separate PR. S7 calls poolaccess directly, S5 passes the package-level admitExperimentPool, the S3 write-back was stated and tested (T7), messages, risks, docs and the release note were rewritten for public by default, and line references were rebased on 3ce0d87b9.
  - Revision 3. A verifier and a critic checked revision 2 against 3ce0d87b9 and found eight problems. All eight were accepted; two were fixed differently from the proposed fix.
      1. W1 was too weak: in basic mode any user may patch any mutable workspace's defaults, so a granted non-admin could point a shared workspace at a restricted pool. W1 now requires an admin for a restricted pool (CanSetWorkspaceDefaultPool, message 5), checks only a changed value in PatchWorkspace, and also covers PostWorkspace, which the proposed fix did not mention.
      2. S8 ran after the metadata write. S8 is now a preflight before any write, with CanSetExperimentsMaxSlots moved into it. The proposed fix (move the :1076 config load earlier and reuse it) was not taken, because SaveExperimentConfig would then write back a config read before the metadata patch and undo it. The preflight uses a separate read.
      3. Section 3 called the upgrade behaviour-neutral. It now lists the changes that apply to public pools too (deltas a-f), and the release note matches a-d.
      4. Section 6 called the searcher's call in experiment.go the only newTrial call site; restore.go:197 is a second, exempt one.
      5. Several line references were wrong (PostWorkspace, ErrStaticResourcePoolConflict, the CreateGenericTask transaction). They are corrected.
      6. S3 wrapped a resolution failure with errors.Wrapf, which surfaces as Unknown. It now returns InvalidArgument, like the first resolution.
      7. A TensorBoard on another user's experiment runs that user's image with the launcher's token. This became gap 5 and D18. The condition was narrowed from "the launcher owns every selected experiment" to "the launcher owns the experiment whose image is inherited", because only that experiment's image, pull secrets and registry_auth are inherited.
      8. The zero-slot case with a non-Ready aux default was described as "accepted before". Before, the experiment was saved and then failed to start; now it fails before it is saved, and ValidateOnly still passes it.

    Tests T2, T6, T7, T13 and T14 were extended or rewritten for these changes, R7 and R11 were reworded, and R14 now describes W1 friction.
