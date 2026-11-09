:orphan:

**Bug Fixes**

-  Cluster usage: the usage of a task still running when its day was aggregated was missing from
   that day. Days are now recomputed until every task overlapping them has ended.

-  Cluster usage: the usage history is rebuilt from the task records at the first start after the
   upgrade, which may take some minutes. Usage of tasks whose records were deleted before is no
   longer in it. Usage is also recorded by user id.
