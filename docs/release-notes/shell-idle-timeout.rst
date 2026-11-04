:orphan:

**Improvements**

-  Shell: A shell whose configuration sets ``idle_timeout`` ends after that long without SSH
   sessions, browser terminals, requests to its services through the master, or JupyterLab activity
   as ``services.jupyter.idle_type`` defines it (by default, an executing kernel).
