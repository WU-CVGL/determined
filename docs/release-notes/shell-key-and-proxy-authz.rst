:orphan:

**Security Fixes**

-  API: **Important:** The master no longer accepts a task's session token as a user's login token,
   or a login token as a task's session token. Before this change, such a token could authenticate
   as an unrelated session that had the same internal ID, which could belong to another user,
   including an administrator. Tokens used as intended are unaffected.

-  Shell: **Important:** Shell listings no longer include each shell's SSH private key, and
   ``GET /api/v1/shells/{id}`` returns the key only to the user who started the shell or to an
   administrator, under every authorization mode. Before this change, any signed-in user could read
   the key of every shell and connect to it as the shell's user. Other users can still see a
   shell's details, but ``det shell open`` and ``det shell show-ssh-command`` now stop with an
   error for them. ``det shell start`` is unchanged.

-  Notebook: **Important:** Notebook listings no longer include the Jupyter token in each
   notebook's address, and ``GET /api/v1/notebooks/{id}`` includes it only for the user who started
   the notebook or an administrator, under every authorization mode. Before this change, any
   signed-in user could read the token of every notebook and run code in it as the notebook's
   user. The WebUI and ``det notebook open`` now fetch the token when the owner opens or connects
   to a notebook. Other users see a message that only the owner or an administrator can open it.
   Launching a notebook from the WebUI or with ``det notebook start`` is unchanged.

-  Shell, Notebook: The master logs an info-level message each time an administrator reads the SSH
   key of another user's shell or the Jupyter token of another user's notebook.

-  API: Only a task's own containers, its owner, or an administrator can set the address that the
   master uses to reach the task's proxied services, under every authorization mode. This is
   ``POST /api/v1/allocations/{id}/proxy_address``. Before this change, any user who could see a
   task could redirect its proxied services to another address. Tasks report this address
   themselves on Kubernetes and Slurm/PBS, and they continue to work. The master now refuses an
   address that is not an IP address, an address for a task that has no resources or has already
   ended, and any address for tasks on agent resource pools, which never send one.

-  API: The same rule now applies to the other calls through which a task's containers report to
   the master: readiness and waiting, all-gather and rendezvous, accelerator data, daemon
   resources, container start notifications, preemption acknowledgements, and pending preemption
   requests under ``/api/v1/allocations/{id}/``. Before this change, any user who could see a task
   could make these calls for it, for example to mark it ready or to have the master stop it.
   Tasks make these calls with their own task session and continue to work.

-  Proxy: Requests that the master forwards through ``/proxy/`` to notebooks, TensorBoards, shells,
   commands, and other task services no longer carry the visitor's Determined session cookies
   (``auth`` and ``det_jwt``). These services run code chosen by the task's owner and could
   otherwise act as any user who opened them. For services that require Determined
   authentication, an ``Authorization: Bearer`` header is removed too, because it can only hold a
   Determined token. The service's own cookies and JupyterLab's ``Authorization: token`` header are
   forwarded as before. Services that allow unauthenticated access still receive any
   ``Authorization`` header that the client sends.
