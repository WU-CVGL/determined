:orphan:

**Security Fixes**

-  API: ``POST /task-logs`` now requires a signed-in session and permission to edit the task, as
   ``POST /api/v1/task/logs`` does. Unmanaged trials, which ship their logs there, must run with a
   signed-in session, as they already do.
