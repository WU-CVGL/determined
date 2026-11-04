:orphan:

**Breaking Changes**

-  Shell: Every shell runs JupyterLab, and VS Code in the browser when the master serves
   code-server, unless its configuration sets the service's ``enabled`` to ``false``. Shells start a
   little slower and use more memory.

**Improvements**

-  CLI: ``det shell start --no-service <name>`` leaves a service out of a shell, and ``--service
   <name>`` turns on one that a template turns off.
-  WebUI: **Launch Interactive Task** checks the services that the shell would run.
