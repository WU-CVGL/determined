.. _vscode:

####################
 Visual Studio Code
####################

You can use VS Code with a shell in two ways: the desktop app over SSH, or VS Code in the browser,
which runs in the shell's container.

**************
 Remote - SSH
**************

#. Make sure `Visual Studio Code Remote - SSH
   <https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-ssh>`__ extension is
   installed. Windows users should run these steps in a Windows shell outside of WSL.

#. Start a new shell and get its SSH command by running:

   .. code::

      det shell start --show-ssh-command

   You can also get the SSH command for an existing shell using:

   .. code::

      det shell show-ssh-command <SHELL UUID>

#. Copy the SSH command, then select ``Remote-SSH: Add new SSH Host...`` from the **Command
   Palette** in VS Code, and paste the copied SSH command when prompted. Finally, you'll be asked to
   pick a config file to use. The default option should work for most users.

#. The remote host will now be available in the VS Code **Remote Explorer**. For further detail,
   please refer to the `official documentation <https://code.visualstudio.com/docs/remote/ssh>`__.

.. _vscode-browser:

************************
 VS Code in the Browser
************************

A shell can run VS Code in the browser, `code-server <https://github.com/coder/code-server>`__, as
its ``vscode`` service, next to its SSH server. It runs with the same user, files and environment as
the SSH sessions. The master must serve code-server releases (see :ref:`master-config-interactive`).

#. Start a shell with the service:

   .. code::

      det shell start --service vscode

#. Once the service is ``READY``, which ``det task services <shell id>`` shows, open the editor:

   .. code::

      det task open <shell id> vscode

   This opens the WebUI page ``/det/tasks/<shell id>/services/vscode/open`` in the browser, which
   logs in to the editor and opens it on the shell's working directory. You can also open that page
   directly.

Only the user who started the shell and administrators can open its editor.

Where code-server Comes From
============================

A shell runs the code-server release that the master served when the shell was created, which the
service takes from the first of:

-  ``code-server`` on the image's ``PATH``, when it is that release. The service skips another
   release on the ``PATH`` and says so in the task log.

-  The cache in the home directory, ``~/.cache/determined/code-server/<version>/linux-<arch>``, or
   in the directory that ``DET_CODE_SERVER_CACHE`` names. The cache holds each release for each
   architecture, next to the lock file ``<version>/linux-<arch>.lock`` of its install.

-  The master: the service downloads the release for the container's architecture, checks its
   SHA-256, and installs it into the cache. Shells that share a home directory install a release
   once, also when they start at the same time.

An administrator can override this release for every task: mount a directory that holds an extracted
release into the containers read-only, for example with ``bind_mounts`` in
``task_container_defaults``, and set ``DET_CODE_SERVER_DIR`` to its path in the containers'
environment. When that directory has ``bin/code-server``, the service uses it before the sources
above without checking its release, which must be 4.14.1 or later.

The service supports glibc 2.28 or later, the baseline of this integration; code-server's own
minimum differs by release.

Keeping Downloads, Extensions and Settings
==========================================

The download is in ``~/.cache``, and the editor keeps its extensions and settings in
``~/.local/share/code-server``. Unless the home directory is on storage that outlives the shell,
they go away with it. Bind-mount the whole home directory, or at least these two directories, from
shared storage, so that the download, the extensions and the settings persist across shells and
agents. For example, with the paths of your home directory on the shared storage and in the
container:

.. code:: yaml

   bind_mounts:
     - host_path: /shared/home/alice/.cache
       container_path: /home/alice/.cache
     - host_path: /shared/home/alice/.local/share/code-server
       container_path: /home/alice/.local/share/code-server

When the Service Is UNAVAILABLE
===============================

The ``vscode`` service is ``UNAVAILABLE`` when the shell cannot run code-server: the image lacks
what it needs (see :ref:`shell-image-requirements`), the cache cannot be written, or the master does
not serve the release. The shell's task log, which ``det task logs <shell id>`` prints, names the
reason in its last line that starts with ``vscode service:``. The shell and its SSH server keep
running.

The service downloads code-server over the shell's connection to the master: a shell whose
environment sets ``http_proxy`` or ``https_proxy`` must list the master in ``no_proxy``.
