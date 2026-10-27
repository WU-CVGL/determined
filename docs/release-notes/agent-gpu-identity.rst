:orphan:

**Bug Fixes**

-  Agent: Reattach task containers to the GPUs they were started on after an agent restart when
   ``visible_gpus`` or ``exclude_gpus`` leaves gaps in the slot IDs, instead of another GPU or a
   crash.

-  Agent: Kill a task container that cannot be reattached after an agent restart, for example
   because its slot ID now belongs to a GPU with another UUID, instead of leaving it running.

**Improvements**

-  Master: Refuse an agent that reports a GPU that an agent with another agent ID already reports.
