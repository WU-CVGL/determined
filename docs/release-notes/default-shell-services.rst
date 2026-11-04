:orphan:

**Breaking Changes**

-  Shell: Every shell now runs JupyterLab, and VS Code in the browser where the master serves
   code-server, unless its configuration or template sets the service's ``enabled`` to ``false``
   (``det shell start --no-service <name>``). Shells start a little slower and use more memory.

**Improvements**

-  CLI: ``det shell start --no-service <name>`` leaves a service out of the shell; ``--service
   <name>`` turns one on that a template turns off.
-  WebUI: The service checkboxes of **Launch Interactive Task** are checked for the services the
   shell would run.
