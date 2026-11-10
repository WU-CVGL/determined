:orphan:

**Bug Fixes**

-  Cluster usage: the usage of a task still running when its day was aggregated was missing from
   that day. Days are now recomputed until every task overlapping them has ended.

-  Cluster usage: the usage history is rebuilt from the task records at the first start after the
   upgrade, which may take some minutes. Usage of tasks whose records were deleted before is no
   longer in it. The aggregates from before the upgrade are kept in the table
   ``resource_aggregates_legacy``.
