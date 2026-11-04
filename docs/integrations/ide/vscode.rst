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

A shell runs VS Code in the browser, `code-server <https://github.com/coder/code-server>`__, as its
``vscode`` service, next to its SSH server, when the master serves code-server releases (see
:ref:`master-config-interactive`). It runs with the same user, files and environment as the SSH
sessions. In the WebUI's **Launch Interactive Task** form, **vscode** is then checked; uncheck it,
or pass ``--no-service vscode`` to ``det shell start``, to leave it out.

#. Start a shell:

   .. code::

      det shell start

#. Once the service is ``READY``, which ``det task services <shell id>`` shows, open the editor:

   .. code::

      det task open <shell id> vscode

   This opens the WebUI page ``/det/tasks/<shell id>/services/vscode/open`` in the browser, which
   logs in to the editor and opens it on the shell's working directory. You can also open that page
   directly.

In the WebUI, the shell's page has a **VS Code** tab: on the **Jobs** page or a workspace's **Jobs**
tab, click the shell's name, or choose **Open VS Code** from its action menu. The tab opens the
editor once the service is ``READY``; see :ref:`shell-services`. Only the user who started the shell
and administrators can open its editor.

Where code-server Comes From
============================

A shell runs the code-server release that the master served when the shell was created, which the
service takes from the first of:

-  ``code-server`` on the image's ``PATH``, when it is that release. The service skips another
   release on the ``PATH`` and says so in the task log.

-  The cache: the directory that ``DET_CODE_SERVER_CACHE_DIR`` names in the shell's environment;
   else ``/var/cache/determined/code-server`` when it exists in the container (see
   :ref:`vscode-keep`); else ``~/.cache/determined/code-server``. A cache holds each release for
   each architecture in ``<version>/linux-<arch>``.

-  The master: the service downloads the release for the container's architecture, checks its
   SHA-256, and installs it into the cache. Shells that share a cache install a release once, also
   when they start at the same time, except for shells that cannot take the install's lock (shells
   of different users on NFSv4, or an image without ``flock``), which may each download it; one copy
   is kept. When the release is not in ``/var/cache/determined/code-server`` and cannot be installed
   there, the service installs it into ``~/.cache/determined/code-server``. A line of the task log
   that starts with ``vscode service:`` names the directory.

An administrator can override this release for every task: mount a directory that holds an extracted
release into the containers read-only, for example with ``bind_mounts`` in
``task_container_defaults``, and set ``DET_CODE_SERVER_DIR`` to its path in the containers'
environment. When that directory has ``bin/code-server``, the service uses it before the sources
above without checking its release, which must be 4.14.1 or later.

The service supports glibc 2.28 or later, the baseline of this integration; code-server's own
minimum differs by release.

.. _vscode-keep:

Keeping Downloads, Extensions and Settings
==========================================

A release of code-server takes about 700 MB, and a shell downloads it again unless its cache
outlives the shell. To keep one copy for all users, mount a directory of shared storage at
``/var/cache/determined/code-server`` in every task container, with ``bind_mounts`` in the
:ref:`task_container_defaults <master-task-container-defaults>` of the master configuration:

.. code:: yaml

   task_container_defaults:
     bind_mounts:
       - host_path: /shared/cache/code-server
         container_path: /var/cache/determined/code-server

The first shell that needs a release installs it there and the others use it, whichever user they
run as, so the directory must be writable by those users, for example with mode ``1777``. Each user
then runs code-server as installed by another user. Where users do not trust each other, make the
directory writable by an administrator only, who installs each release from a shell of their own;
the other shells use it, or their home cache while the release is missing. A user can name a cache
of their own, for example on shared storage that their shells mount, with
``DET_CODE_SERVER_CACHE_DIR`` in ``environment.environment_variables``.

A shell installs a release in a temporary directory ``<version>/linux-<arch>.tmp.<suffix>`` next to
it and removes only its own, also when the shell's services stop during the install. A shell that is
killed during an install, for example with ``det shell kill``, can leave one behind; remove it while
no shell installs that release.

The editor keeps its extensions and settings in ``~/.local/share/code-server``, which goes away with
the shell unless it is on shared storage. ``~`` is ``/root`` in a shell that runs as root. In a
shell that runs as a linked agent user (see :ref:`run-as-user`), ``~`` is the task's working
directory, ``/run/determined/workdir`` unless ``work_dir`` names another. For such a user, set
``work_dir`` to a directory of shared storage that the shell mounts:

.. code:: yaml

   work_dir: /shared/home/alice
   bind_mounts:
     - host_path: /shared/home/alice
       container_path: /shared/home/alice

Or mount a directory of shared storage at ``<work_dir>/.local``, here
``/run/determined/workdir/.local``. The host directory must exist and belong to the linked agent
user. A mount at a deeper path, such as ``.local/share/code-server``, leaves the directories above
it owned by root, so that the user cannot create others there, such as ``~/.local/bin``.

When the Service Is UNAVAILABLE
===============================

The ``vscode`` service is ``UNAVAILABLE`` when the shell cannot run code-server: the image lacks
what it needs (see :ref:`shell-image-requirements`), the cache cannot be written, or the master does
not serve the release. The shell's task log, which ``det task logs <shell id>`` prints, names the
reason in its last line that starts with ``vscode service:``. The shell and its SSH server keep
running.

The service downloads code-server over the shell's connection to the master: a shell whose
environment sets ``http_proxy`` or ``https_proxy`` must list the master in ``no_proxy``.
