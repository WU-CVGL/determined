:orphan:

**New Features**

-  WebUI: Open a terminal in a running shell from the browser. On the **Tasks** page, click the
   shell's name or choose **Open Terminal** from its action menu. The master connects to the
   shell's SSH server with the shell's own key, which never reaches the browser, and relays the
   terminal over a WebSocket on ``/ws/shells/<shell ID>/terminal``. Only the user who started the
   shell and administrators can open its terminals, under every authorization mode; workspace
   permissions such as RBAC's ``Editor`` role do not allow it. A terminal is a new SSH session like
   ``det shell open``: closing the tab ends it, and the page asks before leaving a connected
   session. See :ref:`shell-web-terminal`.

-  Master: The new ``shell_terminal`` section of the master configuration sets whether
   administrators can open terminals in other users' shells, how many terminals can be open per
   user and in total, the idle timeout, the maximum session length, how often open terminals are
   checked against the user's login session and permissions, and the reverse proxies whose
   ``X-Real-IP`` header the terminal's audit log trusts. The master logs every terminal that opens
   and closes, with its user and duration, and records in the shell's task log when a terminal
   opens. Add ``-shell_terminal`` to ``feature_switches`` to turn the terminals off. See
   :ref:`shell_terminal <master-config-shell-terminal>`.

**Improvements**

-  Shells: The SSH server in shells now probes idle clients every 30 seconds
   (``ClientAliveInterval 30``). Idle ``det shell open`` sessions stay connected through proxies
   with short read timeouts, and the server drops clients that stopped answering.
