.. _job-configuration-reference:

#############################
 Job Configuration Reference
#############################

The behavior of :ref:`shells <shells>`, the interactive job type, and of batch :ref:`commands
<commands-and-shells>` and :ref:`TensorBoards <tensorboards>` can be influenced by setting a variety
of configuration variables. These configuration variables are similar but not identical to the
configuration options supported by :ref:`experiments <experiment-config-reference>`.

Configuration settings can be specified by passing a YAML configuration file when launching the
workload via the Determined CLI:

.. code::

   $ det tensorboard start experiment_id --config-file=my_config.yaml
   $ det cmd run --config-file=my_config.yaml ...
   $ det shell start --config-file=my_config.yaml

Configuration variables can also be set directly on the command line when any Determined task,
except a TensorBoard, is launched:

.. code::

   $ det cmd run --config description="determined_command" ...
   $ det shell start --config resources.priority=1

Options set via ``--config`` take precedence over values specified in the configuration file.
Configuration settings are compatible with any Determined task unless otherwise specified.

The following configuration settings are supported:

-  ``description``: A human-readable description of the task. This does not need to be unique. The
   default description consists of a timestamp and the entrypoint of the command.

-  ``environment``: Specifies the environment of the container that is used to execute the task.

   -  ``image``: The Docker image to use when executing the workload. This image must be accessible
      via ``docker pull`` to every Determined agent machine in the cluster. Users can configure
      different container images for NVIDIA GPU tasks using ``cuda`` key (``gpu`` prior to 0.17.6),
      CPU tasks using ``cpu`` key, and ROCm (AMD GPU) tasks using ``rocm`` key. Default values:

      -  ``determinedai/pytorch-ngc-dev:0736b6d`` for NVIDIA GPUs and for CPUs.
      -  ``determinedai/environments:rocm-5.0-pytorch-1.10-tf-2.7-rocm-0.26.4`` for ROCm.

      For TensorFlow users, we provide an image that must be referenced in the experiment
      configuration:

      -  ``determinedai/tensorflow-ngc-dev:0736b6d`` for NVIDIA GPUs and for CPUs.

   -  ``force_pull_image``: Forcibly pull the image from the Docker registry and bypass the Docker
      cache. Defaults to ``false``.

   -  ``environment_variables``: A list of environment variables that will be set in every trial
      container. Each element of the list should be a string of the form ``NAME=VALUE``. See
      :ref:`environment-variables` for more details. Users can customize environment variables for
      GPU, CPU, and ROCm tasks differently by specifying a dict with ``cuda`` (``gpu`` prior to
      0.17.6), ``cpu``, and ``rocm`` keys.

   -  ``pod_spec``: Only applicable when running Determined on Kubernetes. Applies a pod spec to the
      pods that are launched by Determined for this task. See :ref:`custom-pod-specs` for details.

   -  ``registry_auth``: Defines the default `Docker registry credentials
      <https://docs.docker.com/engine/api/v1.30/#tag/System/operation/SystemAuth>`__ to use when
      pulling a custom base Docker image, if needed.

      -  ``username`` (required)
      -  ``password`` (required)
      -  ``server`` (optional)
      -  ``email`` (optional)

      The master returns a task's ``registry_auth`` only to the user who started the task and to
      administrators. Other users get the task's configuration without it.

   -  ``add_capabilities``: A list of Linux capabilities to grant to task containers. Each entry in
      the list is equivalent to a ``--cap-add CAP`` command-line argument to ``docker run``.
      ``add_capabilities`` is honored by resource managers of type ``agent`` but is ignored by
      resource managers of type ``kubernetes``. See :ref:`master configuration
      <master-config-reference>` for details about resource managers.

   -  ``drop_capabilities``: Just like ``add_capabilities`` but corresponding to the ``--cap-drop``
      argument of ``docker run`` rather than ``--cap-add``.

   -  ``proxy_ports``: Expose configured network ports on the chief task container. See
      :ref:`proxy-ports` for details.

-  ``resources``: The resources Determined allows a task to use.

   -  ``slots``: Specifies the number of slots to use for the task. The default value is ``1``. The
      maximum value is the number of slots on the agent in the cluster with the most slots. For
      example, Determined will be unable to schedule a task that requests 4 slots if the Determined
      cluster is composed of agents with 2 slots each. The number of slots for TensorBoard is fixed
      at ``0`` and may not be changed.

   -  ``shm_size``: The size of ``/dev/shm`` for task containers. The value can be a number in bytes
      or a number with a suffix (e.g., ``128M`` for 128MiB or ``1.5G`` for 1.5GiB). Defaults to
      ``4294967296`` (4GiB). If set, this value overrides the value specified in the :ref:`master
      configuration <master-config-reference>`.

   -  ``priority``: The priority assigned to this task. Tasks with smaller priority values are
      scheduled before tasks with higher priority values. Only applicable when using the
      ``priority`` scheduler. Refer to :ref:`scheduling` for more information.

   -  ``resource_pool``: The resource pool where this task will be scheduled. If no resource pool is
      specified, CPU-only tasks will be scheduled in the default CPU pool, while GPU-using tasks
      will be scheduled in the default GPU tool. Refer to :ref:`resource-pools` for more
      information.

   -  ``devices``: A list of device strings to pass to the Docker daemon. Each entry in the list is
      equivalent to a ``--device DEVICE`` command-line argument to ``docker run``. ``devices`` is
      honored by resource managers of type ``agent`` but is ignored by resource managers of type
      ``kubernetes``. See :ref:`master configuration <master-config-reference>` for details about
      resource managers.

   -  ``prefer_gpu_topology``: ``false``, ``"soft"`` or ``"strong"``: with ``"soft"``, a task with 2
      or more slots gets the best-ranked set of free GPUs of its agent by P2P, NVLinks, PCIe
      switches, NUMA nodes and PCIe link width, for example ``det cmd run --config resources.slots=4
      --config resources.prefer_gpu_topology=soft 'echo $DET_SLOT_IDS'``. In a pool with NUMA
      packing, it also prefers an agent where one NUMA node has as many free GPUs as the task has
      slots: soft may take a pool's emptier agent, including an idle one. With ``"strong"``, it
      waits until one NUMA node of an agent has as many free GPUs as the task has slots and gets
      GPUs of that node. See :ref:`prefer_gpu_topology <exp-config-resources-prefer-gpu-topology>`.

   -  ``gpu_groups``: named groups of GPUs of exact models on one agent, each with ``name``,
      ``count`` and ``models``, for example ``[{name: simulation, count: 1, models: ["NVIDIA GeForce
      RTX 4090"]}, {name: learner, count: 1, models: ["NVIDIA H100 PCIe"]}]``. The
      counts must add up to ``slots``; TensorBoards, which have no slots, cannot set it. The task
      starts only when one agent has free GPUs for every group, and its container gets
      ``DET_GPU_GROUPS`` with each group's GPU UUIDs. See :ref:`gpu_groups
      <exp-config-resources-gpu-groups>`.

-  ``bind_mounts``: Specifies a collection of directories that are bind-mounted into the Docker
   containers for execution. This can be used to allow commands to access additional data that is
   not contained in the command context. This field should consist of an array of entries. Note that
   users should ensure that the specified host paths are accessible on all agent hosts (e.g., by
   configuring a network file system appropriately). Defaults to an empty list.

   -  ``host_path``: (required) The file system path on each agent to use. Must be an absolute
      filepath.

   -  ``container_path``: (required) The file system path in the container to use. May be a relative
      filepath, in which case it will be mounted relative to the working directory inside the
      container. It is not allowed to mount directly into the working directory (``container_path ==
      "."``) to reduce the risk of cluttering the host filesystem.

   -  ``read_only``: Whether the bind-mount should be a read-only mount. Defaults to ``false``.

   -  ``propagation``: (Advanced users only) Optional `propagation behavior
      <https://docs.docker.com/storage/bind-mounts/#configure-bind-propagation>`__ for replicas of
      the bind-mount. Defaults to ``rprivate``.

-  ``work_dir``: Working directory. This can include ``$AGENT_USER`` or ``$DET_USER``, which will be
   replaced with the actual agent user id or determined user id. This cannot be set if submitting a
   context directory. Defaults to null.

-  ``tensorboard_args``: Lists optional arguments for launching TensorBoard. Each element of the
   list should be a string of the form ``NAME=VALUE``.

-  ``idle_timeout``: Specifies the duration before an idle instance is automatically terminated.
   This string is a sequence of decimal numbers, each with optional fraction and a unit suffix, such
   as "30s", "1h", or "1m30s". Valid time units are "s", "m", "h". The duration must be positive.
   This is only used by shells and TensorBoard instances:

   -  Shells: There is no default, and the shortest value is ``10s``; a shell is only terminated for
      being idle when this is set. The idle time counts from the last activity, once no browser
      terminal, SSH connection through the master or WebSocket connection to the shell is open.
      Activity is a browser terminal, an SSH connection through the master, a request to any of the
      shell's services through the master, an open WebSocket connection to one of them, and, for the
      ``jupyter`` service, JupyterLab activity as ``services.jupyter.idle_type`` defines it; by
      default a kernel that is executing. An SSH session or a browser page kept open on the shell
      can therefore delay its termination. Processes that run in the shell are not activity.

   -  TensorBoard: The default is the master's ``tensorboard_timeout``, ``5m`` (5 minutes) unless
      the master sets another. A TensorBoard instance is considered to be idle if it does not
      receive any HTTP traffic.

-  ``services``: Shells only. The services that the shell runs in its container besides its SSH
   server, as a map from the name of each service to an object of its options:

   -  ``enabled``: Whether the shell runs the service. The default is ``true``.

   -  ``idle_type``: ``jupyter`` only. Specifies how to decide whether JupyterLab is idle or active
      in a shell with ``idle_timeout``. The default is ``activity``. Valid values are:

      -  ``kernels_or_terminals``: JupyterLab is considered active if any kernels or terminals are
         running.

      -  ``kernel_connections``: JupyterLab is considered active if there are any open connections
         from any web connections to any kernels. (JupyterLab does not report connections to
         terminals, so they cannot be counted.)

      -  ``activity``: JupyterLab is considered active if any kernel is executing a command or any
         terminal that is currently being viewed in JupyterLab is inputting or outputting any data.
         (A terminal that is running a command but not being viewed or running a command with no
         output is treated as idle, since JupyterLab does not provide activity information for those
         case.)

   -  ``logdir``: ``tensorboard`` only, and required when it runs. The directory that TensorBoard
      reads, a path in the container; a relative one is resolved against the working directory, and
      a missing one is created.

   -  ``args``: ``tensorboard`` only. A list of more TensorBoard arguments, after the service's own.
      The service sets ``--logdir``, ``--port``, ``--path_prefix`` and ``--host``; an argument that
      sets one of them or ``--bind_all`` is refused.

   A service's entry in the launch configuration keeps the template's ``idle_type``, ``logdir`` and
   ``args`` when it does not set them, and runs the service unless it sets ``enabled`` to ``false``;
   an empty ``args`` list clears the template's.

   The services are:

   -  ``jupyter``: JupyterLab.
   -  ``ssh``: The SSH server, which every shell runs; it cannot be disabled.
   -  ``tensorboard``: TensorBoard on ``logdir``. See :ref:`tensorboard-service`.
   -  ``vscode``: VS Code in the browser, code-server. See :ref:`vscode-browser`.

   By default a shell runs ``jupyter``, and ``vscode`` when the master serves code-server (see
   :ref:`master-config-interactive`): the launch adds each of them to the map unless the map, or the
   template's, has it. A service's port is chosen outside the ports of ``environment.proxy_ports``.

   For example, a shell that does not run JupyterLab and runs TensorBoard on ``runs`` in its working
   directory:

   .. code:: yaml

      services:
        jupyter:
          enabled: false
        tensorboard:
          logdir: runs

   See :ref:`shell-services`.

-  ``slurm``: Slurm cluster details may optionally be specified in the same fashion as for
   :ref:`experiments <slurm-config>`.

-  ``pbs``: PBS cluster details may optionally be specified in the same fashion as for
   :ref:`experiments <pbs-config>`.
