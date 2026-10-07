:orphan:

**Improvements**

-  API: Experiments and generic tasks filter by slot counts (``slots``, ``slots_above``) and by
   several workspaces (``workspace_ids``), replacing ``slots_filter``.
-  API: Generic tasks sort by start, end, name, state group, user, resource pool or slots, and list
   the owner's display name.
-  API: Experiments sort by slots and by state group, and sorts by name, user and resource pool fold
   only A-Z and compare by code point, with experiments that have no resource pool last.
