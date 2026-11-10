:orphan:

**New Features**

-  API: ``GET /api/v1/users/{user_id}/resources`` returns the GPUs, CPU slots and auxiliary
   containers a user's tasks hold, and whether auxiliary containers are metered on the cluster
   (``aux_metered``). ``GET /api/v1/users/{user_id}/usage`` returns the user's daily or monthly
   seconds of each in UTC, the slot-seconds of an unknown kind (``unclassified_slot_seconds``), and
   the active time and days for GPUs, auxiliary containers and either. Allocations record when their
   resources were assigned and released and what they count as; earlier allocations count as GPUs
   when their task container recorded GPUs.
