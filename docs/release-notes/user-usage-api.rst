:orphan:

**New Features**

-  API: ``GET /api/v1/users/{user_id}/resources`` returns the GPUs, CPU slots and auxiliary
   containers a user's tasks hold, and ``GET /api/v1/users/{user_id}/usage`` the user's daily or
   monthly seconds of each in UTC, with the active time and days for GPUs, auxiliary containers and
   either. Allocations record when their resources were assigned and released and what they count
   as; earlier allocations take their pool's slot type once it is known.
