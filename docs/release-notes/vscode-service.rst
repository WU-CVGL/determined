:orphan:

**Improvements**

-  Shell: A shell runs VS Code in the browser as the ``vscode`` service when the master serves
   code-server.

-  Shell: The ``vscode`` service keeps code-server in the directory that
   ``DET_CODE_SERVER_CACHE_DIR`` names, or in ``/var/cache/determined/code-server`` when an
   administrator mounts a directory there, one copy for all users.
