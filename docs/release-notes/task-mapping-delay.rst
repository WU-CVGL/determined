:orphan:

**Improvements**

-  Resources: Attribute task metrics only after an allocation has run for
   ``observability.task_mapping_delay`` (default 5 minutes).

**Bug Fixes**

-  Resources: Remove ended allocations from ``/prom/det-state-metrics`` instead of exporting them as
   zero until the master restarts.
