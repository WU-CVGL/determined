:orphan:

**Breaking Changes**

-  Master, API, WebUI: Remove the ``TENSORBOARD`` task and job types and their API,
   ``/api/v1/tensorboards``. The upgrade deletes the records of TensorBoard tasks with their logs,
   so end TensorBoard tasks before upgrading; TensorBoard logs in checkpoint storage are kept. A
   rollback restores the database backup taken before the upgrade. ``TASK_TYPE_TENSORBOARD``,
   ``TYPE_TENSORBOARD``, the ``tensorboards`` count of ``GET /api/v1/tasks/count``, the task state
   ``STATE_WAITING`` and ``POST /api/v1/allocations/{allocation_id}/waiting`` are removed.

-  CLI: ``det tensorboard`` manages viewing sessions only, and an earlier CLI's ``det tensorboard``
   fails. ``determined.common.api.NTSC_Kind`` no longer has ``tensorboard``.

-  Shell: The configuration key ``tensorboard_args`` is ``services.tensorboard.args``. A
   configuration or template that sets ``tensorboard_args`` is refused; ``det tensorboard start``
   still reads it from a configuration file. The upgrade moves it in templates that set
   ``services`` and removes it from other templates and stored configurations.

-  Master: ``tensorboard_timeout`` and the Helm value ``tensorboardTimeout`` are removed. A master
   configuration that sets ``tensorboard_timeout`` is refused. A viewing session ends after its
   ``idle_timeout``.
