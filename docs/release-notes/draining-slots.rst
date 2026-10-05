:orphan:

**Bug Fixes**

-  Agents: A slot that is drained, with ``drain: true`` on ``POST
   /api/v1/agents/{agent_id}/slots/{slot_id}/disable``, no longer receives new tasks. Previously
   the scheduler could give a drained slot to a new task while the slot was idle, after the task
   running on it exited, or after a task that had reserved it was canceled before it started. The
   agent then started the new task on the drained slot. A drained slot now keeps the task that runs
   on it and takes no other. Once it is idle, it no longer counts toward the resource pool's total
   slots, as for a disabled slot. The priority scheduler's preemption feasibility simulation also no
   longer counts devices freed from draining slots as allocatable capacity.

-  Agents: Enabling, disabling or draining a single slot now makes the resource pool schedule again
   at once. Previously a task that waited for a slot stayed queued after the slot was enabled, until
   another event in the pool, such as another task starting or ending, made it schedule.

-  Agents: Enabling a drained slot ends its drain. Previously ``det slot list`` showed such a slot
   as both enabled and draining.

-  Agents: A slot that is disabled while its task runs, and enabled again after the task's
   resources were released but before the agent reported the task's container stopped, now comes
   back free. Previously it stayed counted as used until the agent reconnected.
