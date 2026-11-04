:orphan:

**Breaking Changes**

-  Shell: A new shell runs JupyterLab, and VS Code in the browser when the master serves
   code-server, unless its configuration or template sets the service's ``enabled`` to ``false``. A
   shell restored after a master restart keeps the services it started with. JupyterLab comes from
   the image: without it, the ``jupyter`` service is ``UNAVAILABLE`` and the shell runs on. Shells
   start a little slower and use more memory.

**Improvements**

-  CLI: ``det shell start --no-service <name>`` leaves a service out of a shell, and ``--service
   <name>`` turns on one that a template turns off.
-  WebUI: **Launch Interactive Task** checks the services that the shell would run.
