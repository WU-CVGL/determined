:orphan:

**Bug Fixes**

-  WebUI: Jobs, Tasks and resource pool queues distinguish **Submitted** from **Started**.
   Submitted is when the master registered the job; Started is its first resource assignment (for
   experiments, the first trial assignment, excluding checkpoint cleanup). Image pulling and setup
   can follow assignment. Started is supported only in clusters managed entirely by Agent resource
   managers. Other or mixed backends and jobs without a confirmed assignment show a dash. Resource
   pages also explain the default monitoring delay for shells and notebooks. Jobs, Tasks and the
   resource pool Active tab support sorting Started in either direction, with missing values last.
