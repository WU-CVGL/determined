:orphan:

**Breaking Changes**

-  Master, API, WebUI: Remove the ``NOTEBOOK`` task and job types. The upgrade deletes the records
   of notebooks with their logs, so end notebooks before upgrading. ``TASK_TYPE_NOTEBOOK``,
   ``TYPE_NOTEBOOK`` and the ``notebooks`` count of ``GET /api/v1/tasks/count`` are removed.

-  Shell: The configuration key ``notebook_idle_type`` is ``services.jupyter.idle_type``. A
   configuration or template that sets ``notebook_idle_type`` is refused. Configurations that an
   earlier master returned carry it by default, so remove it from those saved as files or templates.
   The WebUI's recent launches start empty. JupyterLab's idle check reads the container variable
   ``DET_JUPYTER_IDLE_TYPE``.
