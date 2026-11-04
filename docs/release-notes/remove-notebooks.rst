:orphan:

**Breaking Changes**

-  CLI, API, WebUI: Remove notebooks: ``det notebook``, the notebook API (``/api/v1/notebooks``) and
   the WebUI's JupyterLab launch. JupyterLab runs as the ``jupyter`` service of a shell: ``det shell
   start`` and ``det task open <shell id> jupyter``, or **Launch Interactive Task** in the WebUI.
   See :ref:`jupyter-service`.

-  Master: Remove the ``notebook_timeout`` setting. A master configuration that sets it does not
   start, so remove it before upgrading. The Helm chart's ``notebookTimeout`` and ``det deploy aws
   --notebook-timeout`` are removed with it.

**Improvements**

-  Shell: ``det shell start --preview`` prints the configuration that the shell would start with,
   without starting it. ``POST /api/v1/shells/preview`` returns that configuration and its warnings.
