:orphan:

**New Features**

-  WebUI: A resource pool's **Active** tab lists the GPUs each job holds, by agent and slot, and
   outlines a job's GPUs in the topology panel on a click.
-  API: ``GetJobs`` and ``GetJobsV2`` return the slots each job holds, by agent, as ``placement``.
