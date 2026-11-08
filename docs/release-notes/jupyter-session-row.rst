:orphan:

**Bug Fixes**

-  Shell: A shell whose JupyterLab session record could not be written at launch was reported as
   failed but kept its allocation, unlisted and not killable until the master restarted. The master
   no longer keeps separate JupyterLab session records; the token stays in the task's stored
   configuration.
