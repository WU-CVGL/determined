:orphan:

**Improvements**

-  Resource pools: List the pools that a user may not use instead of hiding them. ``GET
   /api/v1/resource-pools`` returns each pool with ``usable``, and the WebUI shows such a pool
   striped and tagged **Restricted** and does not let pool pickers select it. Submissions to it are
   still refused.

-  Resource pools: Hide the pools that a user may not use from the user's pool list, as before, with
   ``resource_pool_access.hide_unusable_pools: true`` in the master configuration.
