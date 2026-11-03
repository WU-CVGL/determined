:orphan:

**Improvements**

-  Shell: A shell whose configuration sets ``idle_timeout`` ends once it has been idle that long.
   Browser terminals, requests to its services through the master, and running JupyterLab kernels
   and terminals (see ``notebook_idle_type``) count as activity.
