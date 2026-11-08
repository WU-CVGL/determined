:orphan:

**Bug Fixes**

-  Shell: A shell whose JupyterLab token could not be recorded at launch was reported as failed but
   kept its allocation, unlisted and not killable until the master restarted. JupyterLab tokens are
   no longer recorded in the database.
