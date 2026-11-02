:orphan:

**Bug Fixes**

-  WebUI: Jobs and Tasks distinguish **Submitted** from **Started**. Submitted is when the master
   registered the job; Started is its first resource assignment (for experiments, the first trial
   assignment). Jobs without a recorded assignment show a dash. Image pulling and setup can follow
   assignment. Resource pages also explain the default monitoring delay for shells and notebooks.
