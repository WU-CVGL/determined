:orphan:

**Breaking Changes**

-  Master, API, WebUI: Remove the ``NOTEBOOK`` task and job types. The upgrade deletes the records
   of notebooks with their logs, so end notebooks before upgrading. ``TASK_TYPE_NOTEBOOK``,
   ``TYPE_NOTEBOOK`` and the ``notebooks`` count of ``GET /api/v1/tasks/count`` are removed.

-  Shell: The configuration key ``notebook_idle_type`` is ``services.jupyter.idle_type``. A
   configuration or template that sets ``notebook_idle_type`` is refused; remove it from the
   configurations of earlier shells before launching them again. JupyterLab's idle check reads the
   container variable ``DET_JUPYTER_IDLE_TYPE``.
