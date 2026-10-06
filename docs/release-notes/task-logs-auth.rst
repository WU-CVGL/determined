:orphan:

**Security Fixes**

-  API: ``POST /task-logs`` now requires a signed-in session and applies the same permission check
   as ``POST /api/v1/task/logs`` (for trials, permission to edit the experiment). Unmanaged trials,
   which ship their logs there, must run with a signed-in session, as they already do.
