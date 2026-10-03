:orphan:

**Security Fixes**

-  Shell: **Important:** Shell listings no longer include each shell's SSH private key, and
   ``GET /api/v1/shells/{id}`` returns the key only to the user who started the shell or to an
   administrator, under every authorization mode. Before this change, any signed-in user could read
   the key of every shell and connect to it as the shell's user. Other users can still see a
   shell's details, but ``det shell open`` and ``det shell show-ssh-command`` now stop with an
   error for them. ``det shell start`` is unchanged.

-  Proxy: Requests that the master forwards through ``/proxy/`` to notebooks, TensorBoards, shells,
   commands, and other task services no longer carry the visitor's Determined session cookies
   (``auth`` and ``det_jwt``). These services run code chosen by the task's owner and could
   otherwise act as any user who opened them. For services that require Determined
   authentication, an ``Authorization: Bearer`` header is removed too, because it can only hold a
   Determined token. The service's own cookies and JupyterLab's ``Authorization: token`` header are
   forwarded as before. Services that allow unauthenticated access still receive any
   ``Authorization`` header that the client sends.
