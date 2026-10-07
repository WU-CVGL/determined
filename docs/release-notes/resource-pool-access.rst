:orphan:

**Breaking Changes**

-  Workspaces: With basic authorization, only a workspace's owner or an administrator can change its
   default pools.

**New Features**

-  Resource pools: Restrict a pool to administrators and the users granted access with ``det rp
   access`` or ``/api/v1/resource-pool-access``. Pools stay public until an administrator restricts
   them, and rolling the master back to an earlier version makes every pool public again. See
   :doc:`/maintenance/resource-pool-access`.

**Improvements**

-  Experiment: Save the resource pool that ``CreateExperiment`` checked in the experiment's config,
   and refuse a zero-slot experiment whose default aux pool is not ready before saving it.

**Bug Fixes**

-  API: Reject a job queue move without a target pool with ``400 Bad Request`` instead of moving the
   job to a default pool.
-  Experiment: Check the pool that an invariant config policy sets also for ``CreateExperiment``
   with ``validate_only``.
