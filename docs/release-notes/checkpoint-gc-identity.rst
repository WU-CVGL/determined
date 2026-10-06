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
   and the experiment's checkpoint storage settings, including the standard ``shared_fs`` mount of
   ``host_path`` at ``/determined_shared_fs``. Credentials or proxies that only the experiment
   provided belong in ``checkpoint_storage`` or ``task_container_defaults``.

-  Checkpoints: ``directory`` storage that the trials had on a mount is collected only if the GC
   task has the same storage there: a task container default bind mount of the same host path, or a
   ``checkpoint_gc_pod_spec`` volume of the same claim, host path or NFS export and ``subPath``. For
   a host path, if the experiment's ``pod_spec`` pins its trials to a node by ``nodeName``, a
   ``kubernetes.io/hostname`` ``nodeSelector`` or a required node affinity on one hostname, the GC
   pod spec must pin the task to the same node. The master reads the experiment's ``pod_spec`` on
   every resource manager, so its volume mounts and node pins also count where pod specs are
   ignored, such as the agent resource manager. Otherwise deleting the experiment ends in
   ``DELETE_FAILED``, retryable once the GC task has the same storage and node (never for other
   volume types), and its checkpoints are kept, also beyond the ``save_*`` settings, with the reason
   in the master log. Trials placed in other ways count as not pinned, and their host paths are
   taken to be the same storage on every node, which is not checked. Storage that the trials did not
   have on a mount, or had on an ``emptyDir`` volume, is handled as before.

-  Checkpoints: Known limitation of this fix: ``shared_fs`` storage is not checked. GC tasks do not
   follow a node that the experiment's ``pod_spec`` pins, so ``host_path`` must be the same storage
   on every node, as its documentation assumes. An experiment bind mount, or on Kubernetes a
   ``pod_spec`` volume mount, over the checkpoint path under ``/determined_shared_fs``, such as a
   bind mount of ``/srv/alice`` at ``/determined_shared_fs/sub`` with ``host_path: /srv/shared`` and
   ``storage_path: sub``, is not detected: the trials wrote to ``/srv/alice``, a GC task sees
   ``/srv/shared/sub``, and GC records those checkpoints as deleted while their files remain. Before
   this change, GC took the experiment's bind mounts, and its ``pod_spec`` unless
   ``checkpoint_gc_pod_spec`` was set, and saw them.
