:orphan:

**Security Fixes**

-  Shell: **Important:** Shell listings no longer include each shell's SSH private key, and
   ``GET /api/v1/shells/{id}`` returns the key only to the user who started the shell or to an
   administrator, under every authorization mode. Before this change, any signed-in user could read
   the key of every shell and connect to it as the shell's user. Other users can still see a
   shell's details, but ``det shell open`` and ``det shell show-ssh-command`` now stop with an
   error for them. ``det shell start`` is unchanged.
