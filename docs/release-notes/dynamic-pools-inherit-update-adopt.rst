:orphan:

**New Features**

-  Resource pools: ``det resource-pool update`` and ``PUT /api/v1/resource-pools/dynamic/{name}``
   replace the configuration of a dynamic resource pool. An update replaces the whole configuration
   and is saved at once. A ``Ready`` pool runs it from the next master start, like an edited
   ``master.yaml`` entry; a ``Failed`` pool becomes ``Pending`` and is initialized with the new
   configuration right away. The optional ``expected_revision`` refuses an update when the pool has
   changed in the meantime. A pool cannot be renamed or deleted. See
   :doc:`/maintenance/dynamic-pools`.

-  Resource pools: ``det resource-pool adopt`` and ``POST
   /api/v1/resource-pools/dynamic/{name}/adopt`` save a pool that ``master.yaml`` configures as a
   dynamic pool, so that it keeps running, with its agents, tasks, and workspace bindings, once its
   entry is removed from ``master.yaml``. The request carries the ``master.yaml`` entry copied
   verbatim. While the entry stays in ``master.yaml``, the master serves the pool from it and logs a
   warning at startup, and it refuses to start if the entry differs from the saved configuration.

-  Resource pools: An agent resource manager accepts ``resource_pools: []``, so that all of its
   pools can be dynamic pools. ``default_compute_resource_pool`` and ``default_aux_resource_pool``
   may name dynamic pools, and the master refuses to start when they name a pool that exists neither
   in ``master.yaml`` nor as a dynamic pool. Omitting ``resource_pools`` adds a pool named
   ``default``, as for any agent resource manager.

**Improvements**

-  Resource pools: A dynamic pool saves the configuration that the administrator wrote and resolves
   it like a ``master.yaml`` pool: a pool without its own ``scheduler`` or
   ``task_container_defaults`` uses the master's, and changes to them in ``master.yaml`` reach the
   pool at the next master start. The pool's effective configuration is saved next to it and shown
   as ``config`` in API responses. Pools created by a master without spec support, such as 0.40.1,
   keep running the effective configuration they were saved with until they are updated.

-  Resource pools: Dynamic pool responses include the saved ``spec``, its ``revision``, the
   ``active_revision`` that the running master runs, ``defined_in_master_yaml``, and
   ``pending_restart``. ``det resource-pool list-dynamic`` shows the revision, the active revision,
   and whether a restart is pending.

**Breaking Changes**

-  Resource pools: Replaying a dynamic pool create with the same idempotency key returns ``409
   Conflict`` once the pool has been updated, and for a pool that a master without spec support
   created. Automation that retries creates must treat this conflict as "already exists" and check
   ``det resource-pool list-dynamic``.

-  Resource pools: A dynamic pool with a saved spec follows changes to the master's ``scheduler``
   and ``task_container_defaults`` in ``master.yaml`` from the next master start, instead of keeping
   the values resolved when it was created. Set a value in the pool's configuration to keep it
   fixed. As in ``master.yaml``, a pool-level ``task_container_defaults`` block resets
   ``shm_size_bytes``, ``network_mode``, and ``preemption_timeout`` to their built-in values unless
   it repeats them.

-  Resource pools: A master without spec support, such as 0.40.1, starts with the same database only
   when no ``master.yaml`` pool, including the ``default`` pool that an omitted ``resource_pools``
   key adds, shares a name with a dynamic pool, and ``master.yaml`` does not set ``resource_pools:
   []``. It runs every dynamic pool from its saved effective configuration, including updates that
   were waiting for a restart.

**Bug Fixes**

-  CLI: ``det resource-pool create`` sends ``Content-Type: application/json``, which the master
   requires for dynamic pool requests. A CLI without this fix, such as the one of 0.40.1, gets ``415
   Unsupported Media Type`` for every create.
