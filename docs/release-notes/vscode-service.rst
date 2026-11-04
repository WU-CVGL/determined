:orphan:

**Improvements**

-  Shell: Shells can run VS Code in the browser as the ``vscode`` service.

-  Shell: The ``vscode`` service keeps code-server in ``/var/cache/determined/code-server`` when an
   administrator mounts a directory there, one copy for all users, or in the directory that
   ``DET_CODE_SERVER_CACHE_DIR`` names.
