:orphan:

**Bug Fixes**

-  Detached mode: When sending an unmanaged trial's output to the master fails or stalls, for
   example because the session expired, the trial no longer hangs while printing or exiting. Output
   is still printed locally, the problem is reported once on stderr, and each line sent to the
   master now carries its own timestamp and the worker's rank.
