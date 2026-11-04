:orphan:

**Breaking Changes**

-  Shell: A new shell runs JupyterLab by default, and VS Code in the browser when the master serves
   code-server, unless its configuration or template sets the service's ``enabled`` to ``false``
   (``det shell start --no-service <name>``). A task restored after a master restart keeps the
   services it was started with. The default does not install JupyterLab into the image: without it,
   the ``jupyter`` service is ``UNAVAILABLE`` and the shell runs on. Shells start a little slower
   and use more memory.

**Improvements**

-  CLI: ``det shell start --no-service <name>`` leaves a service out of the shell; ``--service
   <name>`` turns one on that a template turns off.
-  WebUI: The **JupyterLab** and **VS Code** boxes under **Services** in **Launch Interactive Task**
   are checked for the services the shell would run.
