:orphan:

**Improvements**

-  API: Add ``warnings`` and ``warnings_if_restricted`` to each pool of ``GET
   /api/v1/resource-pool-access``, and ``warnings_if_restricted`` to the responses to changes of
   pool access.

-  Experiment: Activating experiments in bulk and resuming runs check each resource pool's access
   once per request and reuse that result, also a failure to read the access records, for the pool's
   other experiments in the request.
