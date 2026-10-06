:orphan:

**Improvements**

-  Resources: Attribute task metrics only after an allocation has run for
   ``observability.task_mapping_delay`` (default 5 minutes).

**Bug Fixes**

-  Resources: Remove ended allocations and experiments from ``/prom/det-state-metrics`` instead of
   keeping them until the master restarts.
