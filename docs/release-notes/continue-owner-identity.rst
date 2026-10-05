:orphan:

**Security Fixes**

-  API: **Important:** An experiment that someone other than its owner continues now runs as its
   owner. Before this change, an administrator, or under RBAC another user with permission to edit
   the experiment, who continued it with ``det experiment continue`` or **Resume Current Trial** in
   the WebUI ran the owner's code with their own session token, ``DET_USER``, and agent user and
   group (uid/gid), until the master restarted. The owner's code could then act with all of their
   permissions. The continued experiment's trials now get a session token for the owner, the
   owner's agent user and group, and the owner's ``DET_USER``, and the job queue lists the owner, as
   after a master restart. Permission to continue is still checked for the user who continues, and
   continuing your own experiment is unchanged. Forks, including **Continue Trial in New
   Experiment** in the WebUI, still belong to the user who creates them.

-  API: An experiment whose owner is deactivated can no longer be continued, because its trials
   would run as that user; continuing it returns ``400 Bad Request`` (``FailedPrecondition``) and
   names the owner. Reactivate the owner first. On clusters with external sessions, which give
   tasks the token of the request that starts them, only an experiment's owner can continue it.
