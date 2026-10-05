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

.. _shell-web-terminal:

Terminals in the WebUI
======================

You can also open a terminal in a running shell from the WebUI: on the **Jobs** page or a
workspace's **Jobs** tab, click the shell's name, or choose **Open Terminal** from its action menu.
The terminal opens in a new browser tab. Only the user who started the shell and administrators can
open terminals in it; when an administrator opens one, the shell's task log records it.

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
