:orphan:

**New Features**

-  Resource pools: Per-pool access. Every resource pool is public unless an administrator restricts
   it; a restricted pool can be used only by administrators and the users granted access (``det
   resource-pool access``, or ``det rp access``). Access is checked when work is submitted,
   activated, unpaused, resumed, or continued, when a job is moved to another pool, and when a
   workspace default pool or the pool of an experiment config policy is set. Pools a user cannot use
   are hidden from ``GET /api/v1/resource-pools``. Administrators manage access with ``det rp access
   list``, ``set``, ``grant``, and ``revoke``, or through ``/api/v1/resource-pool-access``. See
   :doc:`/maintenance/resource-pool-access`.

**Breaking Changes**

-  Resource pools: After the upgrade, every pool stays public until an administrator restricts it.
   Non-administrator submissions, pool lists, and workspace default changes now read the access
   tables, and fail with ``503 Service Unavailable`` when the database cannot be read;
   administrators are unaffected.

-  Resource pools: Access is stored in the database tables ``resource_pool_restrictions`` and
   ``resource_pool_grants``. Restoring a database dump taken before the upgrade, or rolling back to
   a master without resource pool access, makes every pool public. Save the output of ``det rp
   access list --json`` with each database dump to restrict and grant again from it.

-  Workspaces: With basic authorization, only a workspace's owner or an administrator can change its
   default pools. Before, any user could.

-  API: Job queue moves with an empty pool name are rejected with ``400 Bad Request``, instead of
   moving the job to a default pool.

-  Experiment: ``CreateExperiment`` with ``validate_only`` now also checks the resource pool set by
   an invariant config policy. A pool that is not ready or not available to the workspace fails
   validation with ``400 Bad Request``.

**Improvements**

-  Experiment: ``CreateExperiment`` saves and returns the resource pool that it checked as
   ``resources.resource_pool`` of the experiment's configuration, also when the request omitted it.
   A zero-slot experiment that omits its pool while the cluster's default aux pool is not ready now
   fails before it is saved, instead of being saved and then failing to start.
