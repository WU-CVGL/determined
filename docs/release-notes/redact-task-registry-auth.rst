:orphan:

**Security Fixes**

-  API: **Important:** The configuration of a notebook, TensorBoard, shell, command, or generic task
   now includes ``environment.registry_auth`` only for the user who started the task and for
   administrators, under every authorization mode. This applies to ``GET /api/v1/notebooks/{id}``,
   ``GET /api/v1/tensorboards/{id}``, ``GET /api/v1/shells/{id}``, ``GET /api/v1/commands/{id}``,
   and ``GET /api/v1/tasks/{id}/config``, and so to ``det notebook config``, ``det tensorboard
   config``, ``det shell config``, ``det cmd config``, ``det task config``, and the WebUI. Before
   this change, any user who could see a task could read the container registry username and
   password of the user who started it. Other users now get the configuration without
   ``registry_auth``. The rest of the configuration, and the configuration that the task runs with,
   are unchanged.

-  API: **Important:** The same rule applies to the configuration of an experiment in ``GET
   /api/v1/experiments/{id}`` (both ``config`` and ``original_config``), ``GET
   /api/v1/experiments``, and ``POST /api/v1/experiments-search``, and so to ``det experiment
   config`` and the WebUI. Only the experiment's owner and administrators see its ``registry_auth``.

-  API: **Important:** The experiment configuration that a checkpoint or model version carries in
   ``training.experiment_config`` now has ``environment.registry_auth`` set to ``null`` for every
   user, including the experiment's owner and administrators. This applies to ``GET
   /api/v1/checkpoints/{uuid}``, ``GET /api/v1/experiments/{id}/checkpoints``, ``GET
   /api/v1/trials/{id}/checkpoints``, the model version endpoints under ``/api/v1/models``, and
   ``GET /experiments/{id}/preview_gc``, and so to the SDK's
   ``Checkpoint.training.experiment_config`` and the WebUI. Before this change, any user who could
   see a checkpoint could read the container registry username and password of the experiment's
   owner. Nothing is launched from this copy of the configuration; the owner and administrators
   still get ``registry_auth`` from the experiment's own configuration.

-  Generic Tasks: A generic task forked by a user who is neither the original task's owner nor an
   administrator, for example with ``det task fork``, no longer takes the ``registry_auth`` of the
   original task. It uses the ``registry_auth`` in its own configuration or in the task container
   defaults. Likewise, an experiment that such a user forks in the WebUI, with **Fork** or
   **Continue Trial**, no longer carries the owner's ``registry_auth``. The WebUI's **Launch Again**
   and **Start from** for notebooks and shells never copied ``registry_auth`` and are unchanged.

-  API: The master logs an info-level message each time an administrator reads the ``registry_auth``
   of another user's notebook, TensorBoard, shell, command, or generic task.
