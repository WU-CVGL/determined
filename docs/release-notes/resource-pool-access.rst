:orphan:

**New Features**

-  Resource pools: Per-pool access. Every resource pool is public unless an administrator restricts
   it; a restricted pool can be used only by administrators and the users granted access (``det
   resource-pool access``, or ``det rp access``). Access is checked when work is submitted,
   activated, unpaused, resumed, or continued, when a job is moved to another pool, and when a
   workspace default pool is set. Pools a user cannot use are hidden from ``GET
   /api/v1/resource-pools``. Administrators manage access with ``det rp access list``, ``set``,
   ``grant``, and ``revoke``, or through ``/api/v1/resource-pool-access``. See
   :doc:`/maintenance/resource-pool-access`.

**Breaking Changes**

-  Resource pools: After the upgrade, every pool stays public until an administrator restricts it.
   Non-administrator submissions, pool lists, and workspace default changes now read the access
   tables, and fail with ``503 Service Unavailable`` when the database cannot be read;
   administrators are unaffected.

-  Workspaces: With basic authorization, only a workspace's owner or an administrator can change its
   default pools. Before, any user could.

-  API: Job queue moves with an empty pool name are rejected with ``400 Bad Request``, instead of
   moving the job to a default pool.

-  Experiment: ``CreateExperiment`` with ``validate_only`` now also checks the resource pool set by
   an invariant config policy. A pool that is not ready or not available to the workspace fails
   validation with ``400 Bad Request``.
