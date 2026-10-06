:orphan:

**Improvements**

-  Shells: Shell task logs no longer show ``Attempt to write login records by non-root user
   (aborting)`` when each terminal session starts and ends. The SSH server runs as the task's user,
   so it cannot write login records, and the message was harmless.
