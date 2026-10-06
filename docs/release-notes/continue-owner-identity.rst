:orphan:

**Security Fixes**

-  Experiment: **Important:** An experiment that someone other than its owner continues now runs as
   its owner. This applies to every action that continues or retries an existing experiment: ``det
   experiment continue`` in the CLI, and **Resume Current Trial**, **Continue Experiment** (grid and
   random searches), and **Retry** or **Retry Errored** in the WebUI. Before this change, an
   administrator, or under RBAC another user with permission to edit the experiment, who continued
   it ran the owner's code with their own session token, ``DET_USER``, and agent user and group
   (uid/gid), until the master restarted. The owner's code could then act with all of their
   permissions. The continued experiment's trials now get a session token for the owner, the owner's
   agent user and group, and the owner's ``DET_USER``, and the job queue lists the owner. Permission
   to continue is still checked for the user who continues, and continuing your own experiment is
   unchanged. Forks, including **Continue Trial in New Experiment** in the WebUI, still belong to
   the user who creates them.

-  Experiment: **Important:** When someone other than an experiment's owner continues it, the
   override config (``det experiment continue --config`` or ``--config-file``, or the config edited
   in **Resume Current Trial**) can change only ``name``, ``description``, ``labels`` (which, as
   when editing them, can also set the Slurm ``--wckey`` or PBS project), ``max_restarts``,
   ``searcher.max_length`` (deprecated; it extends training only for code that still reads it), and
   the ``checkpoint_storage`` counts ``save_experiment_best``, ``save_trial_best``, and
   ``save_trial_latest``. Every other field must keep its value. Fields such as ``hyperparameters``,
   ``data``, ``entrypoint``, ``environment``, ``bind_mounts``, ``resources.resource_pool``, and
   ``slurm`` choose what the owner's trials run, load, and run with, so anyone else who changed them
   could run code of their choice as the owner. Such a continue returns ``403 Forbidden``
   (``PermissionDenied``), names the changed fields and the owner, and starts nothing. This applies
   to administrators too. Sending back the experiment's whole config unchanged, as **Resume Current
   Trial** does, still works, unless the config has ``data.secrets``, which the WebUI sends back
   masked; then use ``det experiment continue`` without ``--config``, or ask the owner. To run a
   changed copy as yourself, fork the experiment. The owner can still change every field.

**Bug Fixes**

-  Experiment: A continue that fails before its experiment starts, for example **Retry** while the
   experiment is still running, no longer leaves a session open for the experiment's owner.

**Breaking Changes**

-  Experiment: An experiment whose owner is deactivated can no longer be continued or retried, by
   anyone, because its trials would run as that user. Continuing it returns ``400 Bad Request``
   (``FailedPrecondition``) and names the owner. Before this change, an administrator could continue
   it, and it ran as the administrator. Reactivate the owner first, or fork the experiment to run it
   as yourself.

-  Experiment: On clusters with external sessions, which give tasks the token of the request that
   starts them, only an experiment's owner can continue it. Anyone else, administrators included,
   gets ``400 Bad Request`` (``FailedPrecondition``). Before this change, the experiment ran with
   the token of the user who continued it.
