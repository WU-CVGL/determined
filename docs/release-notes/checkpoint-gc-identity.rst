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
   mounts or pod spec. They use the task container defaults of their resource pool (for the pod
   spec, ``checkpoint_gc_pod_spec``, else ``cpu_pod_spec``, merged over ``gpu_pod_spec`` as before)
   and the experiment's checkpoint storage settings, including the ``shared_fs`` mount. Credentials
   or proxies that only the experiment provided belong in ``checkpoint_storage`` or
   ``task_container_defaults``. ``directory`` storage that the trials had on a mount is collected
   only if the GC task has the same storage there: a task container default bind mount of the same
   host path, or a ``checkpoint_gc_pod_spec`` volume of the same claim, host path or NFS export and
   ``subPath``. Otherwise deleting the experiment ends in ``DELETE_FAILED``, retryable once that
   mount is set (never for other volume types), and its checkpoints are kept, also beyond the
   ``save_*`` settings, with the reason in the master log.
