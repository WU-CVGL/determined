:orphan:

**Security Fixes**

-  Checkpoints: **Important:** Checkpoint garbage collection now runs as the experiment's owner,
   with the owner's agent user and group and no user session token, whoever starts it. Before this
   change, a GC task started by deleting another user's checkpoints, experiment or TensorBoard files
   ran with the session token of the user who deleted them, and the experiment's environment
   variables and bind mounts, such as ``BASH_ENV``, ``LD_PRELOAD`` or ``PYTHONPATH``, could run code
   of the owner's choice in it that acted through the API as that user, including an administrator.
   Who may delete is checked as before.

-  Checkpoints: Checkpoint GC tasks no longer take the experiment's environment variables, bind
   mounts or pod spec. They use the task container defaults of their resource pool, with its
   ``checkpoint_gc_pod_spec`` or else its ``cpu_pod_spec``, and the experiment's checkpoint storage
   settings, including the ``shared_fs`` mount. Credentials or proxies that checkpoint GC needs and
   that only an experiment's ``environment_variables`` or ``bind_mounts`` provided belong in the
   checkpoint storage settings or in ``task_container_defaults``. For ``directory`` checkpoint
   storage, the master now refuses to start a GC task when no task container default bind mount or
   pod spec mounts the directory, rather than let the task record the checkpoints as deleted while
   their files remain.
