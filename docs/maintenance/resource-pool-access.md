# Resource pool access

Every resource pool is public unless an administrator restricts it. A
restricted pool can be used only by administrators and by the users granted
access to it. Access is checked when work enters a pool, not while it runs, and
it works the same with basic authorization and with RBAC. It adds to workspace
bindings and RBAC permissions; it replaces neither.

## The rule

A user may start new work in a pool when any of these holds:

- The user is an administrator: the user may update the master
  configuration, which with basic authorization means `admin` is set. This is
  checked first and reads no access record, so administrators keep working when
  the access records cannot be read.
- The pool has no restriction. Such a pool is public. This holds for every pool,
  whether it is configured in `master.yaml` or saved as a dynamic pool.
- The pool is restricted and the user has a grant on it.

Everything else is refused. A restricted pool without grants is for
administrators only. When the access records cannot be read, a non-administrator
is refused with `503 Service Unavailable` and the message
`could not check access to resource pool "<pool>": <error>; try again`; the pool
is never treated as public.

Access never chooses another pool. A refused submission names the pool that was
checked, which is the pool the work would have run in, after workspace and
cluster defaults and invariant config policies applied:

```text
user "alice" may not use resource pool "gpu-a100": the pool is restricted; choose another
pool or ask an administrator for access (if resources.resource_pool was not set, "gpu-a100"
is the default pool for this workspace or the cluster)
```

## What is checked

Access is checked as the user who makes the request, whenever work enters a
pool:

- Launching a command, shell, notebook, or TensorBoard, including the preview
  of a notebook launch.
- Creating a generic task, including a fork and a child task, and unpausing a
  generic task.
- Creating an experiment, including a fork, a clone, `validate_only`, and a
  pool that a template or an invariant config policy sets; continuing an
  experiment; activating an experiment, one at a time or in bulk; and resuming
  runs.
- Moving a job to another pool in the job queue. A move must name its target
  pool.
- Setting a new default compute or aux pool of a workspace.

Requests that code in a task makes, such as a submission from inside a running
experiment, are checked as the user who launched the task.

`GET /api/v1/resource-pools` lists only the pools that the user may use, so the
WebUI's pool pickers and cluster pages, the CLI, and the SDK show only those.
Administrators see every pool. Other lists that name pools, such as workspace
bindings, the job queue, jobs, and agents, are not filtered. `det job list`
without `-r` fails with "the default compute pool is not available to you; name
a pool with -r" when the user may not use the default compute pool.

## What is not checked

Work that a pool has accepted keeps its access:

- Experiments, trials, commands, notebooks, shells, TensorBoards, and generic
  tasks that the master restores after a restart.
- Trials of an accepted experiment, including the trials that its searcher
  creates, and restarts after a failure or preemption.
- The resumption of a generic task tree that an earlier unpause already saved,
  on retry or at master startup.
- Changes to the priority, weight, or `max_slots` of accepted work. Raising
  `max_slots` lets an accepted experiment use more slots of its pool.

Checkpoint garbage collection is not checked either. It runs in the cluster's
default aux pool even when that pool is restricted, and it runs with the
experiment's environment variables and bind mounts. Code that these settings
make it run, for example through `PYTHONPATH` or `LD_PRELOAD`, runs in that pool
too. Restricting the cluster's default aux pool therefore does not keep a user
without a grant from running CPU work there this way: garbage collection starts
when an experiment ends with checkpoints to delete, and when a user deletes
checkpoints or an experiment's TensorBoard files.

## Revocation

Restricting a pool or revoking a grant applies from the next request; a request
whose access was checked before the change was saved is not stopped. Nothing
that runs is killed, paused, or moved, and workspace defaults that name the pool
stay. A user without access can no longer submit, activate, unpause, continue,
fork, or clone work into the pool, move a job into it, or make it a new
workspace default; work that one of their running tasks submits is refused too.
The pool disappears from the user's pool list, while their own experiment and
task pages keep working.

To stop work that already runs in the pool, an administrator pauses it, kills
it, or moves it to another pool in the job queue. Activating a paused
experiment again is checked. Setting `max_slots` to 0 does not revoke anything:
the owner can raise it again.

A running task of a deactivated user can still make requests as that user, with
that user's grants. Kill the task as well as deactivating the user.

## Default pools

A submission that omits `resources.resource_pool` runs in the workspace's
default pool, or in the cluster's default pool when the workspace sets none:
the default compute pool for work with slots, and the default aux pool for work
without slots, such as TensorBoards, CPU commands, notebooks and shells, and CPU
generic tasks.

Administrators may restrict a default pool. Every submission that omits its
pool and would run there is then refused for users without a grant, while
checkpoint garbage collection still runs in the cluster's default aux pool (see
"What is not checked"). The response to the restriction warns about each
default the pool is, and `det resource-pool access list` shows them in its
`Defaults` column, including when a `master.yaml` edit makes a restricted pool
a default.

### Who may change workspace defaults

With basic authorization, only a workspace's owner and administrators may
change its default pools. With RBAC, the permission to set a workspace's
default resource pool is needed.

In every mode, a new default must be a pool that the user who sets it may use:
administrators and public pools pass, and a restricted pool needs that user's
grant. This also applies to the defaults given when a workspace is created.
Unsetting a default, or sending its current value again, is not checked.

With basic authorization, a restricted pool therefore becomes a workspace
default only through an administrator, or through the workspace's owner holding
a grant; nobody else can point a shared workspace at a restricted pool. With
RBAC, it becomes the default of an existing workspace through any user who may
use the pool and has the permission to set the workspace's default resource
pool, whether or not that user owns the workspace.

## Pools and names

Access is recorded by pool name, and nothing deletes the records of a pool
automatically:

- A new dynamic pool is public once it is `Ready`. To avoid a public window,
  restrict its name before creating it: any name may be restricted, also one
  that is not a pool yet. Restricting it while it is `Pending` also works, as a
  pool accepts no work before it is `Ready`.
- Adopting a `master.yaml` pool as a dynamic pool keeps its access.
- A pool that is removed and configured again under the same name, including
  a pool whose conversion to a dynamic pool is rolled back, gets its records
  back.
- A renamed `master.yaml` pool is a new, public pool. The old name keeps its
  records and is listed with `exists` false. Remove them by making the old name
  public and revoking its grants.

A grant on a public pool is stored and has no effect until the pool is
restricted. Making a pool public keeps its grants, which apply again when the
pool is restricted again.

## Grant, then restrict

To restrict a pool that users already rely on without refusing any of them in
between, grant the users first and restrict the pool afterwards:

```sh
det resource-pool access grant gpu-a100 alice bob
det resource-pool access set gpu-a100 --mode restricted
```

## Running another user's code

A task authenticates as the user who launched it, so the requests that its
code makes carry that user's grants and, for an administrator, every
administrator power, including changing pool access. When you launch a task
that runs an image or code that another user chose, you lend that user your
session. You do this on purpose when you:

- fork or clone another user's experiment, which reuses its model definition;
- fork another user's generic task, or create a child task that inherits its
  context;
- launch with another user's template that sets the container image.

Continuing another user's experiment, as an administrator or, with RBAC, as a
user allowed to update it, also runs the experiment owner's code with the
session and the agent user and group of the user who continues it.

Deleting another user's checkpoints or TensorBoard files starts checkpoint
garbage collection with your session and with that experiment's environment
variables and bind mounts, which can make it run code that the experiment's
owner chose.

A TensorBoard takes an experiment's image, image pull secrets, and
`registry_auth` only when the user who starts it owns the experiment.

## Backup, rollback, and upgrade

Access is stored in two database tables, `resource_pool_restrictions` and
`resource_pool_grants`. Upgrading creates them empty, so no pool changes who
may use it until an administrator restricts it. Non-administrator submissions,
pool lists, and changes of workspace defaults read the tables, and fail with
`503 Service Unavailable` when the database cannot be read; administrators are
not affected.

Pool access relies on the TensorBoard rule in "Running another user's code": a
grant makes a user's session worth more, so a TensorBoard must not run another
user's image with it. Every master with pool access has that rule.

Restoring a database dump taken before the tables existed, or rolling back to a
master without pool access, which ignores the tables, makes every pool public.
Rolling forward to a master with pool access restores the restrictions as they
were in the database. Save the output of
`det resource-pool access list --json` with each database dump, and restrict
and grant again from it after such a restore.

## Error messages

A request that access refuses, or cannot decide, fails with one of these
messages. The REST API returns the HTTP status in parentheses.

- `PermissionDenied` (`403`): `user "<user>" may not use resource pool
  "<pool>": the pool is restricted; choose another pool or ask an
  administrator for access (if resources.resource_pool was not set, "<pool>" is
  the default pool for this workspace or the cluster)`.
- `Unavailable` (`503`): `could not check access to resource pool "<pool>":
  <error>; try again`, and for the pool list `could not check access to
  resource pools: <error>; try again`. The access records could not be read.
- `InvalidArgument` (`400`): `moving a job to another resource pool requires
  the target pool name`, for a job-queue move that names no pool.
- `Internal` (`500`): `resource pool access checked before the pool was
  resolved`. This is a bug in the master.
- `PermissionDenied` (`403`), with basic authorization, when a user who is
  neither the workspace's owner nor an administrator changes its default pools:
  `only admins may set other user's workspaces default resource pools`.

Activating experiments in bulk and resuming runs report a refusal in the result
of each refused experiment and activate the others. The admin API's errors are
listed with its endpoints below.

## REST API

These endpoints require an authenticated session cookie or bearer token.
Listing requires authorization to read the master configuration; the other
endpoints require authorization to update it. With basic authorization, both
are restricted to administrators. Request bodies must be JSON with
`Content-Type: application/json`, at most 64 KiB, and without unknown fields.

### List

```text
GET /api/v1/resource-pool-access
```

The response is `{"resource_pools":[...]}`, one item per name, sorted by name.
It lists every pool of every resource manager, including dynamic pools in any
state, and every name that has a restriction or a grant:

```json
{
  "pool_name": "gpu-a100",
  "mode": "restricted",
  "exists": true,
  "default_compute": false,
  "default_aux": false,
  "workspace_defaults": [
    {"workspace_id": 4, "workspace": "vision", "kind": "compute"}
  ],
  "users": [
    {"id": 7, "username": "alice", "active": true, "admin": false}
  ],
  "restricted_at": "<timestamp>",
  "restricted_by": "admin"
}
```

- `mode` is `restricted` exactly when the pool has a restriction.
- `exists` reports that a resource manager configures the pool in
  `master.yaml` or that it is saved as a dynamic pool. A name with records but
  no pool has `exists` false.
- `default_compute` and `default_aux` report that the pool is a cluster default
  pool of its resource manager; `workspace_defaults` lists the workspaces whose
  default compute or aux pool it is.
- `users` are the users granted access, also while the pool is public.
- `restricted_at` and `restricted_by`, a username, are `null` for a public
  pool. `restricted_by` is also `null` once that user is deleted.

### Restrict or make public

```text
PUT /api/v1/resource-pool-access/gpu-a100
Content-Type: application/json

{"mode": "restricted"}
```

`mode` is `restricted` or `public`. Making a pool public keeps its grants. Both
are idempotent.

### Grant and revoke

```text
POST /api/v1/resource-pool-access/gpu-a100/grant
POST /api/v1/resource-pool-access/gpu-a100/revoke
Content-Type: application/json

{"usernames": ["alice", "bob"]}
```

Grants are stored whether the pool is public or restricted. When any username
is unknown, the request returns `404` naming every unknown one, for example
`unknown users: bob, carol; nothing was changed`, and nothing is written. Both
are idempotent.

### Responses to changes

Every change returns `200` with the item and `warnings`, a list of messages:

- `no resource pool named "<pool>" exists; the setting applies to a pool
  created with this name`, when `exists` is false;
- while the pool is restricted, one for each default it is, for example
  `"<pool>" is the cluster's default compute pool: submissions that omit
  resources.resource_pool are refused for users without a grant on "<pool>"`,
  and likewise for the cluster's default aux pool and for each workspace that
  uses it as a default.

The master logs each change at `INFO` level with the administrator who made it.
An invalid `mode`, an empty `usernames` list, an unknown field, or more than one
JSON value returns `400`, a body that is not labeled as JSON `415`, and a body
over 64 KiB `413`.

## CLI

`det resource-pool access`, or `det rp access`, manages access. The WebUI has
no page for it.

```sh
det resource-pool access list
det resource-pool access list --json
det resource-pool access set gpu-a100 gpu-h100 --mode restricted
det resource-pool access set gpu-a100 --mode public
det resource-pool access grant gpu-a100 alice bob
det resource-pool access revoke gpu-a100 bob
```

`list` shows each name's `Mode`, whether it `Exists`, the `Defaults` it is
(`cluster compute`, `cluster aux`, `<workspace> compute`, `<workspace> aux`),
and the `Users` granted access, marking inactive users and administrators.
`--json` prints the API response. `set`, `grant`, and `revoke` print the
master's warnings to standard error. `set` changes each pool in turn, continues
past a pool that fails, and exits with status 1 if any failed.
