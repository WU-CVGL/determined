.. _tensorboards:

###################
 Using TensorBoard
###################

`TensorBoard <https://www.tensorflow.org/tensorboard>`__ is a popular tool for visualizing and
inspecting deep learning models. In Determined, you can use TensorBoard to examine individual
experiments or compare multiple experiments.

*****************
 Getting Started
*****************

Launch TensorBoard instances via the WebUI or the Determined CLI. Before launching TensorBoard
instances from the CLI, :ref:`install the CLI <install-cli>` on your development machine.

In the WebUI, **TensorBoard** on an experiment's or trial's page, and **View in TensorBoard** for
experiments or trials selected in a list or a comparison view, start a TensorBoard task without GPUs
that ends 5 minutes after its last request, and open it on the task page's **TensorBoard** tab. Your
own running TensorBoard of the same experiments or trials in the same workspace opens instead of a
new one. When the default resource pool or the workspace cannot be used, start it from **Launch
Interactive Task** with **TensorBoard** and **Determined experiments**.

***********************
 Analyzing Experiments
***********************

To analyze one or more experiments, use ``det tensorboard start <experiment-id> [<experiment-id>
...]``. It starts a TensorBoard task without GPUs that reads the experiments' TensorBoard logs,
waits until TensorBoard answers, and opens the task's TensorBoard page:

.. code::

   $ det tensorboard start 7
   Launched tensorboard (id: <task-id>, name: TensorBoard (<name>)).
   tensorboard (id: <task-id>) is ready.
   Tensorboard is running at: http://<master>/det/tasks/<task-id>/interactive?tab=tensorboard

Each trial is a run named ``experiment/<id>/trial/<trial-id>``. The experiments must use the same
checkpoint storage; otherwise the start is refused. When the checkpoint storage configuration holds
credentials, only the experiment's owner or an administrator can start a TensorBoard on it.

TensorBoard opens while the logs load and shows new logs as the experiments write them.

Only the user who started a TensorBoard and administrators can open it, and only while they may view
the artifacts of every selected experiment.

***********************************
 Customizing TensorBoard Instances
***********************************

Determined allows you to initialize TensorBoard with a job configuration (YAML) file. This can be
useful for running TensorBoard with a specific container image or for enabling access to additional
data through a bind-mount.

``--config-file`` and ``--config`` set the task's configuration, such as ``environment.image``,
``bind_mounts``, ``resources.resource_pool`` and ``services.tensorboard.args``. Example job
configuration file:

.. code:: yaml

   environment:
     image: determinedai/pytorch-ngc-dev:0736b6d
   bind_mounts:
     - host_path: /my/agent/path
       container_path: /my/container/path
       read_only: true
   services:
     tensorboard:
       args: ['--samples_per_plugin=images=100']

``services.tensorboard.args`` provides optional TensorBoard arguments. In the example above, we set
the maximum number of images to display to 100, overriding TensorBoard's default value.

The image needs TensorBoard 2.9 or later; the task does not install it. Without it, the task ends
and its log says why.

For detailed configuration settings, refer to the :ref:`job-configuration-reference`.

To launch TensorBoard with a configuration file, use ``det tensorboard start <experiment-id>
--config-file=my_config.yaml``.

To manage the task, use the **Jobs** page, or ``det tensorboard list``, ``det tensorboard open
<task-id>``, ``det tensorboard logs <task-id>``, ``det tensorboard config <task-id>`` and ``det
tensorboard kill <task-id>``; they also manage TensorBoard tasks, but not a shell with a
``tensorboard`` service. As the task is a shell, the ``det shell`` and ``det task`` commands also
manage it. ``det tensorboard list --json`` prints TensorBoard tasks as ``/api/v1/tensorboards``
returns them and viewing sessions as ``/api/v1/shells`` does.

Logs on ``shared_fs`` storage are read in place, read-only. For ``type: directory`` storage, give
the TensorBoard the experiment's ``bind_mounts``.

***************************
 Analyzing Specific Trials
***************************

Determined also supports analyzing specific trials from one or more experiments. This can be useful
for comparing a small number of trials from an experiment with many trials, or for comparing trials
from different experiments.

To analyze specific trials, use ``det tensorboard start --trial-ids <trial-id> [<trial-id> ...]``,
alone or with experiment IDs. A trial of a listed experiment is read with its experiment.

.. _data-in-tensorboard:

*********************
 Data in TensorBoard
*********************

This section provides a brief overview of how Determined captures data from TensorFlow models. For a
more in depth discussion on how TensorBoard visualizes data, consult the `TensorBoard documentation
<https://github.com/tensorflow/tensorboard/blob/master/README.md>`__.

TensorBoard visualizes data captured during model training and validation, which is stored in
tfevent files. These files are generated by writing `TensorFlow summary operations
<https://www.tensorflow.org/api_docs/python/tf/summary>`__ to disk using a `tf.summary.FileWriter
<https://www.tensorflow.org/versions/r1.15/api_docs/python/tf/summary/FileWriter>`__. Each deep
learning framework has support for writing and upload metrics as tfevent files.

FileWriters are configured to write log files, called tfevent files, to a directory known as the
``logdir``. TensorBoard monitors this directory for changes and updates accordingly. Training code
takes its ``logdir`` from the context API (see :ref:`tensorboard-additional-events`) and does not
hardcode or build it: Determined chooses the directory for each worker.

**************************
 Determined Batch Metrics
**************************

At the end of every training workload, batch metrics are collected and stored in the database,
providing a granular view of model metrics over time. Batch metrics will appear in TensorBoard under
the Determined group. The x-axis of each plot corresponds to the batch number.

For example, a point at step 5 of the plot is the metric associated with the fifth batch seen.

**********************************
 Framework-Specific Configuration
**********************************

To configure TensorBoard for a specific framework, follow the examples below:

TensorFlow Keras
================

For models using :class:`~determined.keras.DeterminedCallback`, include a
:class:`determined.keras.TensorBoard` callback in your ``model.fit()`` call.:

PyTorch
=======

See :func:`PyTorchTrialContext.get_tensorboard_writer()
<determined.pytorch.PyTorchTrialContext.get_tensorboard_writer>`

**********************************
 TensorBoard Lifecycle Management
**********************************

To manage the task, use the **Jobs** page, or ``det tensorboard list``, ``det tensorboard open
<task-id>``, ``det tensorboard logs <task-id>``, ``det tensorboard config <task-id>`` and ``det
tensorboard kill <task-id>``; they also manage TensorBoard tasks, but not a shell with a
``tensorboard`` service. As the task is a shell, the ``det shell`` and ``det task`` commands also
manage it. ``det tensorboard list --json`` prints TensorBoard tasks as ``/api/v1/tensorboards``
returns them and viewing sessions as ``/api/v1/shells`` does.

************************
 Implementation Details
************************

Determined runs TensorBoard in a task without GPUs; the master proxies its requests. Logs in S3, GCS
or Azure storage are copied into the task's container; logs on ``shared_fs`` or ``directory``
storage are read in place.

TensorBoard listens behind the master's proxy: the proxy adds a token that the task was given to the
requests that it passes on, and TensorBoard refuses requests without it, in every network mode.

.. _tensorboard-additional-events:

***************************************
 Logging Additional TensorBoard Events
***************************************

Any additional TFEvent files that are written to the appropriate path during training are accessible
to TensorBoard. The appropriate path varies by worker rank and can be obtained by one of the
following functions:

-  For CoreAPI and Keras users: :func:`~determined.core.TrainContext.get_tensorboard_path`
-  For PyTorchTrial users: :func:`~determined.pytorch.PyTorchTrialContext.get_tensorboard_path`
-  For DeepSpeedTrial users:
   :func:`~determined.pytorch.deepspeed.DeepSpeedTrialContext.get_tensorboard_path`
