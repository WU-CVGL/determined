:orphan:

**Breaking Changes**

-  CLI, API, WebUI: Remove notebooks (``det notebook``, ``/api/v1/notebooks`` and the WebUI's
   JupyterLab launch); JupyterLab runs in every shell instead. See :ref:`jupyter-service`.

-  Master: Remove ``notebook_timeout``, the Helm chart's ``notebookTimeout`` and ``det deploy aws
   --notebook-timeout``. A master configuration that sets it does not start, so remove it before
   upgrading.

-  Master: End notebooks before upgrading. The master ends those still running when it starts, and
   ``det task logs <task id>`` still shows their logs.

**Improvements**

-  Shell: ``det shell start --preview`` and ``POST /api/v1/shells/preview`` return the configuration
   that a shell would start with, without starting it.
