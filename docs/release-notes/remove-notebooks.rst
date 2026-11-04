:orphan:

**Breaking Changes**

-  CLI, API, WebUI: Remove notebooks: ``det notebook``, the notebook API (``/api/v1/notebooks``) and
   the WebUI's JupyterLab launch. JupyterLab runs as the ``jupyter`` service of a shell: ``det shell
   start --service jupyter`` and ``det task open <shell id> jupyter``, or **jupyter** in the WebUI's
   **Launch Interactive Task** form. See :ref:`jupyter-service`.

-  Master: Remove the ``notebook_timeout`` setting. A master configuration that sets it does not
   start, so remove it before upgrading. The Helm chart's ``notebookTimeout`` and ``det deploy aws
   --notebook-timeout`` are removed with it.

-  Master: A notebook still running when the master is upgraded is ended when the master starts,
   with a line in its task log. End notebooks before upgrading. Ended notebooks stay readable by
   task ID, for example with ``det task logs <task id>``.

**Improvements**

-  Shell: ``det shell start --preview`` prints the configuration that the shell would start with,
   without starting it. ``POST /api/v1/shells/preview`` returns that configuration and its warnings.
