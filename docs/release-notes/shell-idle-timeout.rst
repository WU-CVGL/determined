:orphan:

**Improvements**

-  Shell: A shell whose configuration sets ``idle_timeout`` ends once it has been idle that long.
   SSH sessions, browser terminals and requests to its services through the master count as
   activity, and so does JupyterLab activity as ``services.jupyter.idle_type`` defines it; by
   default a kernel that is executing.
