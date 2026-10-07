# Dynamic resource pools

A dynamic resource pool is a pool of the agent resource manager that is saved
in the database instead of `master.yaml`. An administrator creates one without
restarting the master, changes its configuration through the API, and can save
a `master.yaml` pool as a dynamic pool, so that every pool of an agent resource
manager can be managed this way. Dynamic pools do not create infrastructure:
agents must still be started and configured to join their pool.

## Operation states

A dynamic pool has a durable desired configuration and one of three operation
states:

- `Pending`: the configuration is saved, but runtime initialization has not
  completed.
- `Ready`: the runtime pool is published for agent admission and task
  scheduling.
- `Failed`: initialization failed. The pool remains unavailable until an
  administrator retries it or updates its configuration.

Only `Ready` pools appear through the regular resource-pool listing and can
accept agents or tasks.

## Configuration and inheritance

A dynamic pool saves its spec: the resource-pool configuration exactly as the
administrator wrote it, with only the keys that were sent. The master reads the
spec the way it reads a `master.yaml` pool entry:

- A spec without `scheduler` uses the scheduler of its resource manager.
- A spec without `task_container_defaults` uses the master's
  `task_container_defaults`.
- Pool settings that the spec leaves out take their built-in defaults, for
  example `max_aux_containers_per_agent: 100` and `agent_reconnect_wait: 150s`.

Changes to the master's `scheduler` or `task_container_defaults` in
`master.yaml` reach dynamic pools at the next master start, as they reach
`master.yaml` pools.

Next to the spec, the master saves the pool's effective configuration
(`config` in API responses): the spec resolved against the master
configuration, with the scheduler and task container defaults filled in. It is
written whenever the spec is written, and rewritten at each master start when
the master configuration changes it. The master never runs a pool from it. It
shows what the spec resolves to, and it is what a master without spec support,
such as 0.40.1, runs after a rollback.

A pool saved by a master without spec support has no spec: API responses
omit `spec` and `spec_version`. Such a pool keeps running the effective
configuration it was saved with, so later changes to the master's scheduler,
task container defaults, or built-in defaults do not reach it. An update gives
it a spec; from the next master start, it inherits like any other pool.

### Pool-level task container defaults

A pool's `task_container_defaults` block, even one with a single key, is
merged over the master's `task_container_defaults`, but it starts from
built-in values for three settings. Unless the block repeats them, the pool's
tasks get `shm_size_bytes: 4294967296` (4 GiB), `network_mode: bridge`, and
`preemption_timeout: 3600`, whatever the master sets. This applies to
`master.yaml` pools as well. For example, with a master that sets a 16 GiB
shared memory size, this spec keeps it only because it repeats it:

```yaml
pool_name: batch-a
task_container_defaults:
  add_capabilities: [IPC_LOCK]
  shm_size_bytes: 17179869184
```

Leave the block out entirely to use the master's task container defaults
unchanged.

## Names and limits

Pool names are global across resource managers because normal task admission
routes by pool name. Dynamic names must match
`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. A dynamic pool cannot be renamed or
deleted, and an update cannot change its name or its resource manager. Dynamic
pools do not support provider-backed, Kubernetes, Slurm, or PBS pools. Creating
or updating a pool does not move agents or allocations.

## Access

A new dynamic pool is public: every user may use it once it is `Ready`, until
an administrator restricts it. To avoid a public window, restrict the name
before creating the pool, with
`det resource-pool access set <name> --mode restricted`; a name can be
restricted before it is a pool. Access is recorded by pool name, so updating
or adopting a pool keeps it. See [resource pool access](resource-pool-access.md).

## REST API

These endpoints require an authenticated session cookie or bearer token.
Create, update, adopt, and retry require the same authorization as updating
master configuration; list requires authorization to read master
configuration. With basic authorization, these permissions are restricted to
administrators.

Every request body is limited to 1 MiB and rejects unknown fields. Inside
`config`, unknown fields are rejected at its top level, in `scheduler`, and in
`task_container_defaults` and its `registry_auth` and `kubernetes` objects.
Keys nested deeper, for example in a bind mount, are not checked: an unknown
one is saved in the spec and has no effect, so check the returned `config`,
which shows what the pool runs. `config.provider` is rejected even when it is
otherwise valid master configuration. `cluster_name` may be omitted when
exactly one agent resource manager is configured.

### Create

```text
POST /api/v1/resource-pools/dynamic
Content-Type: application/json

{
  "cluster_name": "agent-cluster",
  "idempotency_key": "create-batch-a",
  "config": {
    "pool_name": "batch-a",
    "description": "Static agents for batch work"
  }
}
```

The master validates `config` as it validates a `master.yaml` pool entry,
resolves its effective configuration, and saves both before runtime
initialization. An invalid spec or effective configuration returns `400`. The
saved spec has its keys sorted and its top-level `null` values removed, so
`"scheduler": null` means the same as leaving `scheduler` out.

A new operation returns `201`. A replay with the same cluster-scoped
idempotency key, pool name, and spec returns `200` and the saved pool. Reusing
the key for a different request, or using a name that is taken, returns `409`.
A replay also returns `409` once the pool's spec has been updated, and for a
pool created by a master without spec support; automation that retries creates
must treat that `409` as "already exists" and check the list. Keys that start
with `adopt:` are reserved for adopted pools.

A master-owned worker initializes saved `Pending` pools. A create, update, or
retry response can contain `Pending`; poll the list endpoint until the pool
becomes `Ready` or `Failed`. The worker rescans saved operations periodically,
so an insert committed just before a failed read or canceled request is still
advanced without restarting the master. It also reconstructs a `Ready` pool
whose runtime was not published after an ambiguous database write, retrying
transient runtime failures. Exact replays return the current state and do not
start another runtime pool.

### Responses

Every endpoint returns pools in this form:

```json
{
  "cluster_name": "agent-cluster",
  "pool_name": "batch-a",
  "config_version": 1,
  "config": {},
  "spec": {
    "description": "Static agents for batch work",
    "pool_name": "batch-a"
  },
  "spec_version": 1,
  "revision": 1,
  "state": "Ready",
  "created_at": "<timestamp>",
  "updated_at": "<timestamp>",
  "active_revision": 1,
  "defined_in_master_yaml": false,
  "pending_restart": false
}
```

- `config` is the full effective configuration computed from the saved spec
  and the master defaults when the spec was last written or the master last
  started. If that computation fails at a start, the master logs a warning
  and keeps the previous `config`, which then lags behind the spec. `Ready`
  means that the pool's runtime is published, not that every task default of
  the pool resolved at this start.
- `spec` is the saved spec. It is absent for a pool saved without a spec.
- `revision` starts at 1 and increases with every change of the spec.
- `active_revision` is the revision that the running master runs. It is `null`
  for a pool that is not published and for a pool that `master.yaml` serves.
- `defined_in_master_yaml` reports that `master.yaml` also configures the
  name, which is the case for an adopted pool until its entry is removed.
- `pending_restart` reports that the running master does not run the saved
  revision: the pool is defined in `master.yaml`, or it is published with an
  `active_revision` other than `revision`.
- `error` is present when initialization failed.

A durable `Ready` written just before the runtime pool is published is shown
as `Pending`. Registry passwords and tokens are redacted in both `config` and
`spec`; `spec` keeps only the keys that were saved.

### List

```text
GET /api/v1/resource-pools/dynamic
GET /api/v1/resource-pools/dynamic?cluster_name=agent-cluster
```

The response is `{"resource_pools":[...]}` in creation order. It includes
pools in every state; the normal resource-pool API includes only ready pools.

### Update

```text
PUT /api/v1/resource-pools/dynamic/batch-a?cluster_name=agent-cluster
Content-Type: application/json

{
  "expected_revision": 1,
  "config": {
    "pool_name": "batch-a",
    "description": "Static agents for batch and debug work",
    "agent_reconnect_wait": "10m"
  }
}
```

An update replaces the whole spec; it is not a patch. Keys that the new spec
leaves out are no longer set and fall back to their defaults, exactly as when a
`master.yaml` entry is edited. Nothing compares the new spec with the old one,
so send every key the pool should keep. `config.pool_name` must equal the name
in the path. A registry credential equal to the redaction placeholder
`********`, as copied from an API response, is rejected; send the credential
itself. The response's `config` is the effective configuration of the new spec
and serves as a preview of what the pool will run.

An update is saved at once, but the running master keeps the runtime pool it
has. What happens depends on the saved pool. The rows are checked from the top,
and the first one that applies decides:

| Saved pool | Result |
| --- | --- |
| Configured in `master.yaml`, saved as a dynamic pool | `409`, even for the same spec; remove the entry from `master.yaml` and restart first. |
| Configured in `master.yaml`, not saved | `404`; adopt the pool first. |
| No dynamic pool, or one of another resource manager | `404`. |
| Same spec as the request | `200`; nothing is written and `revision` is unchanged, even when `expected_revision` is stale. |
| `Pending`, or `Ready` but not yet published | `409`; retry when the pool is `Ready` or `Failed`. |
| `expected_revision` differs from `revision` | `409`, naming the current revision. |
| Changed by another request since it was read | `409`; read the pool again and retry. |
| `Ready` | `200`; `revision` increases and `pending_restart` becomes `true`. The pool runs the new spec from the next master start. |
| `Failed` | `200`; `revision` increases, the pool becomes `Pending`, and the worker initializes the new spec right away. |

`expected_revision` is optional. Until the next master start, the regular
resource-pool API and the WebUI keep showing the values that the pool runs. A
master start applies every pending update at once, so check `pending_restart`
before restarting. An update to a pool saved without a spec gives it a spec, and
it inherits master defaults from the next start.

### Adopt a master.yaml pool

Adopting saves a pool that `master.yaml` configures as a dynamic pool, so that
the pool keeps running once its entry is removed from `master.yaml`:

```text
POST /api/v1/resource-pools/dynamic/batch-b/adopt?cluster_name=agent-cluster
Content-Type: application/json

{
  "config": {
    "pool_name": "batch-b",
    "description": "GPU agents",
    "agent_reconnect_wait": "10m"
  }
}
```

`config` is the pool's `master.yaml` entry, copied verbatim. It must decode to
exactly the configuration that the running master reads from `master.yaml`,
including keys that only repeat a default; otherwise the request returns `400`
and names the top-level keys that differ. A name that the selected resource
manager does not configure in `master.yaml` returns `404`, and a pool with a
`provider` returns `400`. The built-in `default` pool, which the master adds
when the `resource_pools` key is omitted, is adopted with the spec
`{"pool_name": "default"}`.

The pool is saved as `Ready` with revision 1 and the idempotency key
`adopt:<name>`. Nothing changes in the running master, which keeps serving the
pool from `master.yaml`. A replay with the same spec returns `200` and the
saved pool. A spec that decodes to the same configuration but is written
differently, or a name that a dynamic pool already has, returns `409`.

At startup, a saved pool whose name `master.yaml` also configures is accepted
only when it is `Ready`, is saved for the same resource manager, has a spec, and
its spec decodes to exactly the `master.yaml` entry. Then `master.yaml` serves
the pool: the saved pool is not loaded, its effective configuration is not
rewritten, and the master logs the warning `resource pool "<name>" is saved as a
dynamic pool and still defined in master.yaml; remove it from master.yaml`. An
adopted pool is saved `Ready` and stays `Ready` while `master.yaml` serves it.
Every other collision stops startup, including any collision with a pool saved
without a spec or with a `Pending` or `Failed` pool; the error adds that the
`master.yaml` entry of an adopted pool must equal the saved spec or be removed.
Edit the spec with an update only after the entry is removed and the master has
restarted.

Once the entry is removed, the next master start loads the pool from the
database before it restores agents and publishes it before it restores tasks.
Agents, allocations, workspace bindings, templates, and default pool settings
all refer to the pool by name and carry on.

Both checks compare the decoded configurations. The master's configuration
loader lowercases the keys of nested maps in `master.yaml`, so an entry with
such maps, for example in a pod spec, may never compare equal; adopting it
returns `400`. Leave such a pool in `master.yaml`.

### Retry

Retry a failed pool with an empty body:

```text
POST /api/v1/resource-pools/dynamic/batch-a/retry?cluster_name=agent-cluster
```

Retry initializes the saved spec, resolved against the current master
configuration; a pool saved without a spec retries its saved effective
configuration. Retrying a `Pending` or `Ready` pool returns `409`; a missing
pool returns `404`. To retry with a different configuration, update the failed
pool instead.

## Default pools and an empty pool list

`default_compute_resource_pool` and `default_aux_resource_pool` may name
dynamic pools. When either names a pool that is neither configured in
`master.yaml` nor saved as a dynamic pool in any state, the master refuses to
start and names the setting: tasks that name no pool would otherwise be
accepted and then fail to find their pool. To start a cluster before its first
dynamic pool exists, omit the `resource_pools` key, which adds the built-in
`default` pool that both defaults name unless they are set, create or adopt the
pools, then point the defaults at them.

An agent resource manager accepts `resource_pools: []`, so all of its pools
can be dynamic pools. When the `resource_pools` key is omitted, the master
adds a pool named `default` with built-in settings. Agents started
without a `resource_pool` setting join `default`; with `resource_pools: []`
and no dynamic pool named `default`, the master rejects them. A Kubernetes
resource manager requires at least one pool. A master without spec
support refuses to start with `resource_pools: []`.

## Restart behavior

On startup, all saved pools are validated before persisted agent statistics
are cleaned up. The agent resource manager loads every saved pool, whatever
its state, before accepting restored or new agents, publishes it, and sets
its durable state to `Ready`. In the same write, it rewrites the effective
configuration of a pool with a spec when the master configuration changed it.
If that configuration no longer resolves, for example because the merged task
container defaults are invalid, the master logs a warning naming the pool and
keeps the saved one; the pool still runs, and the error surfaces when a task
uses the pool, as for a `master.yaml` pool.

The master refuses to start if a saved pool is corrupt, uses an unsupported
config or spec version, belongs to a non-agent resource manager, collides with
a `master.yaml` pool other than as described for adopted pools, or fails to
initialize. A change to `master.yaml` can make a saved spec invalid, for
example through the scheduler of its resource manager; startup then stops and
names the pool, as for a `master.yaml` entry. This fail-closed behavior avoids
deleting or misrouting saved agent state. Correct the cause before restarting;
the retry and update endpoints are available only while the master is running.

## Convert master.yaml pools to dynamic pools

This procedure moves the pools of an agent resource manager from `master.yaml`
into the database. It needs one master restart besides the upgrade.

Choose the end state before adopting anything, because the built-in `default`
pool and a rollback to a master without spec support constrain it:

| End state | `resource_pools` after step 5 | Pool named `default` | Rollback to a master without spec support |
| --- | --- | --- | --- |
| 1. Every named pool dynamic, rollback kept | key omitted (adds the built-in `default` pool) | not adopted; stays a `master.yaml` or built-in pool | binary swap |
| 2. No static pool at all | `[]` | adopted like the others | no longer a binary swap: such a master refuses `resource_pools: []` |

On the first route the built-in `default` pool is a `master.yaml` pool, so the
cluster is not strictly all dynamic; agents that set no `resource_pool` join
it. A cluster with its own `default` pool in `master.yaml` takes the second
route for that pool, or keeps it in `master.yaml`.

1. Save a database dump, `master.yaml`, and the output of
   `det resource-pool list-dynamic --json`. The listing redacts registry
   credentials, so it serves only to compare configurations later; the dump
   and the original configuration files are the backup.
2. Upgrade the master with `master.yaml` unchanged and restart it. The schema
   migration runs, and every pool keeps running the configuration it had.
3. For each dynamic pool saved without a spec, run
   `det resource-pool update <name> <name>.yaml` with the spec that the pool
   should keep. Include every pool setting that differs from its built-in
   default, such as `agent_reconnect_wait`: an update has no equality check,
   and a setting left out falls back to its default at the next start. Compare
   the returned `config` with the one saved in step 1. They match unless the
   master's scheduler or task container defaults changed since the pool was
   created.
4. Adopt each `master.yaml` pool, in `master.yaml` order, with
   `det resource-pool adopt <name> <name>.yaml`, where `<name>.yaml` is the
   pool's `master.yaml` entry copied verbatim. Pools are listed in the order in
   which they were saved, so this keeps their relative order.
5. Remove the adopted entries from `master.yaml`. On the first route, omit the
   `resource_pools` key when none remain; on the second, set
   `resource_pools: []`. Keep `default_compute_resource_pool`,
   `default_aux_resource_pool`, `scheduler`, and `task_container_defaults`.
   Never remove an entry before its pool is adopted: a master that starts
   without the pool deletes the saved state of its agents and fails the
   experiments that were restoring into it.
6. Restart the master once, in a quiet period. Agents reconnect within their
   pool's `agent_reconnect_wait`.
7. Verify that `det resource-pool list-dynamic` shows every pool `Ready`, with
   `Active` equal to `Revision` and no pending restart; that
   `det dev curl /api/v1/resource-pools` reports `defaultComputePool` and
   `defaultAuxPool` as `true` for the expected pools, which the WebUI cluster
   page labels as default pools; that the master log has no "still defined in
   master.yaml" warning; and that agents and running tasks are present in every
   pool.
8. On the first route, the second can follow once a rollback is no longer
   needed: set `resource_pools: []` to drop the built-in `default` pool.
   Every agent must then set its `resource_pool`.

A pool named `default` needs care: when the `resource_pools` key is omitted, the
master adds a built-in `default` pool. Like a `master.yaml` entry, it serves a
saved `default` pool whose spec has only built-in settings, such as the adopted
built-in pool, with the "still defined in master.yaml" warning, and it stops
startup for any other saved pool of that name. Only `resource_pools: []` lets
the saved pool run, and a master without spec support refuses that setting.
Leave a pool named `default` in `master.yaml` until a rollback is no longer
needed.

## Rollback

A master without spec support, such as 0.40.1, starts with this database only
when:

- no `master.yaml` pool, including the built-in `default` pool added when the
  `resource_pools` key is omitted, shares a name with a saved dynamic pool; and
- `master.yaml` does not set `resource_pools: []`.

Then replace the master binary. That master runs every dynamic pool from its
saved effective configuration as of the last master start or write, so it
applies pending updates and ignores later changes to master defaults. Upgrading
again resumes inheritance and rewrites the effective configurations.

If a rollback is needed after pools were adopted but before their entries were
removed from `master.yaml`, either remove those entries, or delete the saved
rows of exactly those pools. An `adopt:` idempotency key only shows how a row
was created, not whether `master.yaml` still serves the pool: a pool adopted
earlier whose entry is gone exists only in the database, and deleting its row
loses the pool. Delete rows by name only:

1. Stop the master and back up the database.
2. List the pools that `master.yaml` still defines and that the saved rows
   match, which `det resource-pool list-dynamic` showed as
   `defined_in_master_yaml` before the stop.
3. Show the rows to delete and check the count against that list:

   ```sql
   SELECT cluster_name, pool_name, idempotency_key, revision
   FROM dynamic_resource_pools
   WHERE cluster_name = '<cluster>' AND pool_name IN ('<pool-1>', '<pool-2>');
   ```

4. Delete the same rows with the same `WHERE` clause.

The schema has rollback migrations for release rollback tooling. Rolling back
the spec migration drops the saved specs and revisions; every pool then runs
its saved effective configuration, as a pool saved without a spec. Rolling
back the migration that adds dynamic pools drops the table and all saved
pools. Back up the table and stop the master before an intentional migration
rollback if those records must be preserved. A release that predates dynamic
pools cannot restore or schedule them.

## CLI

Save the resource-pool configuration itself as YAML or JSON. For example,
`batch-a.yaml` can contain:

```yaml
pool_name: batch-a
description: Static agents for batch work
```

Create the pool with a stable idempotency key:

```sh
det resource-pool create batch-a.yaml \
  --idempotency-key create-batch-a \
  --cluster-name agent-cluster
```

Replace its spec, optionally only while it has a given revision:

```sh
det resource-pool update batch-a batch-a.yaml --expected-revision 1
```

Adopt a `master.yaml` pool from a file that holds its entry copied verbatim:

```sh
det resource-pool adopt batch-b batch-b.yaml
```

List dynamic pools, or filter them to one resource manager:

```sh
det resource-pool list-dynamic
det resource-pool list-dynamic --cluster-name agent-cluster --json
```

The list shows each pool's `State`, `Revision`, `Active` revision, and
`Pending restart`. `Active` is the revision that the running master runs,
`master.yaml` for a pool that `master.yaml` serves, or `-` for a pool that is
not published.

After correcting the cause recorded on a failed pool, retry its saved spec:

```sh
det resource-pool retry batch-a --cluster-name agent-cluster
```

Omit `--cluster-name` when only one agent resource manager is configured. Add
`--json` to print the complete record. `create`, `update`, and `retry` print
the returned state and exit with status 1 if it is `Failed`, so scripts do not
mistake a saved but uninitialized pool for a usable one.
