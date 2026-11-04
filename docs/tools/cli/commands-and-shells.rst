.. _commands-and-shells:

#####################
 Commands and Shells
#####################

Determined commands and shells provide support for running code on a Determined cluster without
writing a model. This page describes how to manage GPU-powered batch commands and interactive
shells.

Commands and shells are started through the Determined command-line interface (CLI). To learn more,
including installation instructions, visit the :ref:`Determined CLI user guide <cli-ug>` or
:ref:`Determined CLI Reference <cli-reference>`.

Commands execute a user-specified program on the cluster. Commands are useful for running existing
code in batch mode. Shells start SSH servers that let you use cluster resources interactively.
Shells provide access to the cluster in the form of interactive `SSH
<https://en.wikipedia.org/wiki/SSH_(Secure_Shell)>`_ sessions.

**********
 Commands
**********

Determined commands are manipulated with CLI commands starting with ``det command``, abbreviated as
``det cmd``. The main subcommand is ``det cmd run``, which runs a command in the cluster and streams
its output. For example, the following CLI command uses ``nvidia-smi`` to display information about
the GPUs available to tasks in the container:

.. code::

   det cmd run nvidia-smi

You can also run more complex commands including shell constructs provided they are quoted to
prevent interpretation by the local shell:

.. code::

   det cmd run 'for x in a b c; do echo $x; done'

``det cmd run`` streams output from the command until it finishes, but the command continues
executing and occupying cluster resources even if the CLI is interrupted or killed, such as due to
entering ``Ctrl-C``. To stop the command or view additional output, you need the command UUID, which
you can get from the output of the original ``det cmd run`` or ``det cmd list``. After you have the
UUID, run

-  ``det cmd logs <UUID>`` to view a snapshot of logs.
-  ``det cmd logs -f <UUID>`` to view the current logs and continue streaming future output.
-  ``det cmd kill <UUID>`` to stop the command.

.. |br| raw:: html

   <br />

.. _shells:

********
 Shells
********

Shell-related CLI commands start with ``det shell``. To start a persistent SSH server container in
the Determined cluster and connect an interactive session to it, use ``det shell start``:

.. code::

   det shell start

After starting a server with ``det shell start``, you can make another independent connection to the
same server by running ``det shell open <UUID>``. You can get the UUID from the output of the
original ``det shell start`` or ``det shell list`` command:

.. code::

   $ det shell list
    Id                                   | Owner      | Description                  | State   | Exit Status
   --------------------------------------+------------+------------------------------+---------+---------------
    d75c3908-fb11-4fa5-852c-4c32ed30703b | determined | Shell (annually-alert-crane) | RUNNING | N/A
   $ det shell open d75c3908-fb11-4fa5-852c-4c32ed30703b

Optionally, you can provide extra options to pass to the SSH client when using ``det shell start``
or ``det shell open`` by including them after ``--``. For example, this command starts a new shell
and forwards a port from the local machine to the container:

.. code::

   det shell start -- -L8080:localhost:8080

To stop the SSH server container and free cluster resources, run ``det shell kill <UUID>``.

To print the configuration that a shell would start with, merged with its template and the cluster's
defaults, without starting it, add ``--preview``:

.. code::

   det shell start --preview --service jupyter

.. _shell-services:

Services
========

A shell can run more services than its SSH server. Declare them in the shell's configuration with
:ref:`services <job-configuration-reference>`, or with ``--service``, for example JupyterLab:

.. code::

   det shell start --service jupyter

This is the same as ``det shell start --config "services={jupyter: {}}"``. Each service runs in the
shell's container, with the same user, files and environment as the SSH sessions.

To list a shell's services with their states and addresses:

.. code::

   det task services <shell id>

A service is ``UNSPECIFIED`` until the shell reports its state, which also takes a short time after
the master restarts. It is ``STARTING``, then ``READY``; ``EXITED`` once it has stopped, or
``UNAVAILABLE`` when its program is not in the image or cannot be installed (see
:ref:`shell-image-requirements`). A service other than the SSH server can stop without ending the
shell.

To open JupyterLab in the browser once it is ``READY``:

.. code::

   det task open <shell id> jupyter

To start a shell with VS Code in the browser, the ``vscode`` service, and open it (see
:ref:`vscode-browser`):

.. code::

   det shell start --service vscode
   det task open <shell id> vscode

In the WebUI, the shell's page has a tab for each of its services (see :ref:`shell-web-terminal`). A
service's tab opens the service once it is ``READY``; a service that cannot be opened shows its
state in its tab with **Retry**. Only the user who started the shell and administrators can open its
services.

A shell is not terminated for being idle unless its configuration sets :ref:`idle_timeout
<job-configuration-reference>`. With it, open SSH sessions and browser terminals and browser
sessions of its services such as JupyterLab count as activity, as does JupyterLab activity as
``notebook_idle_type`` defines it; by default a kernel that is executing. An SSH session or a
browser page kept open on the shell can therefore delay its termination.

.. _jupyter-service:

JupyterLab
----------

The ``jupyter`` service runs `JupyterLab <https://jupyter.org/>`__ in the shell's container:

.. code::

   det shell start --service jupyter
   det task open <shell id> jupyter

In the WebUI, check **jupyter** in **Launch Interactive Task** on the **Jobs** page or a workspace's
**Jobs** tab, then open it from the **JupyterLab** tab of the shell's page (click the shell's name,
or choose **Open JupyterLab** from its action menu) or with ``det task open``.

Opening a notebook in JupyterLab starts a kernel, which runs until you shut it down in JupyterLab.

A shell is assigned one GPU by default. For JupyterLab without GPUs, set ``resources.slots`` to
``0``:

.. code::

   det shell start --service jupyter --config resources.slots=0

The :ref:`Determined CLI <cli-ug>` is installed in the shell's container, so a notebook cell can run
it, for example ``!det experiment list``.

.. _jupyter-state:

Saving JupyterLab State
-----------------------

Files that are not in a persistent directory are lost when the shell ends. Bind-mount a directory of
a shared filesystem that all agents mount into the shell's container, and keep your notebooks in it.
For example, a user ``jimmy`` with a shared home directory at ``/shared/home/jimmy``:

.. code::

   $ cat > config.yaml << EOL
   bind_mounts:
     - host_path: /shared/home/jimmy
       container_path: /shared/home/jimmy
   EOL
   $ det shell start --service jupyter --config-file config.yaml

A cluster deployed with ``det deploy gcp up``, ``det deploy aws --deployment-type efs`` or ``det
deploy aws --deployment-type fsx`` has a network filesystem that all agents share, mounted into task
containers at ``/run/determined/workdir/shared_fs/``. Set ``work_dir`` to that directory in the
shell's configuration to work there; a :ref:`configuration template <config-template>` can set it
for every launch. A cluster started with ``det deploy local cluster-up --auto-work-dir <directory>``
makes that directory available to task containers as ``./shared_fs``.

A notebook kept on the shared filesystem can be opened again in the JupyterLab of another shell. To
restore its last saved state, use JupyterLab's **File** > **Revert to Checkpoint**. JupyterLab saves
a checkpoint every 120 seconds in an ``.ipynb_checkpoints`` directory next to the notebook; change
``autosaveInterval`` of the **Document Manager** in **Settings** > **Advanced Settings Editor** to
set another interval.

.. _shell-image-requirements:

Image Requirements
==================

A shell's image needs:

-  ``sshd`` at ``/usr/sbin/sshd``.
-  bash 5 or later.
-  ``/proc``.
-  ``jupyter`` on the ``PATH``, for the ``jupyter`` service.
-  glibc 2.28 or later and a writable home directory, for the ``vscode`` service, and ``tar`` and
   ``flock`` to install code-server from the master (see :ref:`vscode-browser`).

The default images provide them. When one is missing, the task log names it.

.. _shell-web-terminal:

Terminals in the WebUI
======================

You can also open a terminal in a running shell from the WebUI: on the **Jobs** page or a
workspace's **Jobs** tab, click the shell's name, or choose **Open Terminal** from its action menu.
The shell's page opens in a new browser tab, with the terminal in its **Terminal** tab next to a
**JupyterLab** and a **VS Code** tab when the shell runs those services (the action menu's **Open
JupyterLab** and **Open VS Code** open those tabs), and the shell's **Logs** and **Resources**. The
terminal waits for the shell to run, connects when its tab is first shown and keeps its session
while you use the other tabs. Only the user who started the shell and administrators can open
terminals in it; when an administrator opens one, the shell's task log records it.

The master connects to the shell's SSH server for you, with the shell's own key, so the key never
reaches the browser. A browser terminal is a new SSH session, like ``det shell open``, and both work
at the same time:

-  Closing the tab, reloading it or losing the connection ends the session and stops the programs
   running in it. The terminal asks before you leave a connected session, but not when its tab
   reloads because another tab of the browser signed in as a different user. To keep long jobs
   running, start them in ``tmux`` (``tmux new -A -s main`` attaches to the same session again) or
   with ``nohup``.

-  **Reconnect** starts a new session in the same shell.

-  A terminal closes after an hour without input or output, after 24 hours, when you sign out, and
   when your login session expires. Each user can have up to eight terminals open. The cluster
   administrator can change these limits; see :ref:`shell_terminal <master-config-shell-terminal>`.

-  Links in the terminal's output open in a new tab only after you confirm the address, and only
   for ``http`` and ``https`` links.

.. _shell-file-locations:

File Locations
==============

When using the ``det shell start`` command with the ``--context`` option:

-  Files are copied to the container to ``/run/determined/workdir``.
-  SSH sessions always start in the HOME directory for the logged-in user.
-  If you don't see your context files immediately, navigate to ``/run/determined/workdir``.

.. note::

   For containers running as root, sessions will start in ``/root``. For containers using
   Apptainer/Singularity, the user's actual system home directory is bind-mounted into the
   container.
