:orphan:

**Improvements**

-  Observability: The master exports a task's mappings on ``/prom/det-state-metrics`` only after its
   allocation has run for ``observability.task_mapping_delay`` (default ``5m``), so tasks that end
   sooner add no per-task series to Prometheus.

**Bug Fixes**

-  Observability: Ended allocations, containers, and experiments no longer stay on
   ``/prom/det-state-metrics`` as zero-valued series until the master restarts.
