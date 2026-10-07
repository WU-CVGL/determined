:orphan:

**Improvements**

-  API: Experiments and generic tasks filter by slot counts (``slots``, ``slots_above``) and by
   several workspaces (``workspace_ids``); ``slots_filter``, added in 0.41.0, is removed.

-  API: Generic tasks sort by start, end, name, state group, user, resource pool or slots, and list
   the owner's display name.

-  API: Experiments sort by slots and by state group, sorts by name, user and resource pool fold
   only A-Z and compare by code point with no resource pool last, and ties under these sorts and
   under start and end time go newest start first, then highest ID.
