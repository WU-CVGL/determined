:orphan:

**Security Fixes**

-  WebUI: **Important:** The master now keeps a browser's session in an ``auth`` cookie that is
   ``HttpOnly`` and ``SameSite=Lax``, wherever it sets one: password sign-in, single sign-on (OIDC
   and SAML), and the legacy ``POST /login?cookie=true``. Scripts cannot read the session token, and
   browsers leave the cookie off requests that other sites start, apart from following a link. The
   WebUI no longer keeps the token in local storage, removes the copy that earlier versions left
   there, and no longer writes the cookie itself. A browser that signed in before the upgrade still
   holds a cookie that scripts can read; the first time the WebUI checks the session, it replaces
   that cookie with the new one, or removes it and asks the user to sign in again if the master no
   longer accepts it. Signing out removes the cookie, even when the session has already ended.

-  WebUI: The session cookie is marked ``Secure`` when the request that sets it arrived over HTTPS:
   the master serves TLS itself, or a reverse proxy listed in the new ``security.trusted_proxies``
   setting sets ``X-Forwarded-Proto`` to ``https``. A reverse proxy that terminates TLS and forwards
   plain HTTP leaves the cookie without ``Secure`` unless it is listed there, or the new
   ``security.session_cookie.secure`` setting is ``always``. Its default, ``auto``, keeps plain-HTTP
   deployments working; ``never`` turns the attribute off. Browsers keep one ``auth`` cookie per
   host name, whatever the port: once a browser holds a ``Secure`` cookie from the HTTPS address,
   signing in over plain HTTP at the same host name, for example on another port, does not work
   until that cookie expires or the user signs out over HTTPS. Reach plain-HTTP ports by another
   host name or address, or leave the cookie without ``Secure``.

-  API: **Important:** Requests that rely on the session cookie must now come from the master's own
   pages when they can change something: every method but ``GET``, ``HEAD``, and ``OPTIONS``, and
   WebSocket connections. The master accepts them when the browser reports the request as
   same-origin, or, over plain HTTP, where browsers do not report it, when the ``Origin`` header
   names the same host and port as the request's ``Host`` header. Other requests get ``403
   Forbidden``, and the master logs a warning with the headers it saw. Requests with an
   ``Authorization: Bearer`` header, as the CLI, the Python SDK, tasks, and scripts send, are not
   affected, except signing in and signing out, which set and remove the cookie: they are checked
   whatever their headers. Requests without an ``Origin`` header, as the CLI and the SDK send, still
   pass.

   When users reach the master over plain HTTP through a reverse proxy, the proxy must forward the
   ``Host`` header as the browser sent it, including the port. In nginx, use ``proxy_set_header Host
   $http_host;``, since ``$host`` drops the port. Otherwise, add the address that users open, such
   as ``http://determined.example.com:8080``, to the new ``security.csrf.trusted_origins`` setting,
   or the WebUI cannot sign in or change anything. A proxy listed in ``security.trusted_proxies``
   may instead pass the original host in ``X-Forwarded-Host``. Over HTTPS, current browsers report
   whether a request is same-origin themselves, and the ``Host`` header does not matter.

-  WebUI: A session token in the WebUI's address (``?jwt=``) is now accepted only when the cluster
   has an external sign-in page, which is what sends it. The WebUI removes the token from the
   address and the browser history before using it, and the master checks it before storing it in
   the session cookie.

-  API: **Important:** Users who change their own password or their own username must now enter
   their current password, so that a session token alone, such as the one that every task carries
   for its owner, can no longer lock users out of their account. This applies to ``SetUserPassword``
   and ``PatchUser`` (``old_password``, hashed like the new password when ``is_hashed`` is set) and
   to the legacy ``PATCH /users/{username}`` and ``PATCH /users/{username}/username`` routes
   (``old_password``). A missing current password gets ``400 Bad Request``, a wrong one ``403
   Forbidden``. Users with a blank password send an empty one. Remote users, who sign in through
   single sign-on, have no password to enter and cannot change their own password or username.
   Administrators changing or renaming other users do not need a password, as before.

-  API: A new password, however it is set, now also revokes all of the user's access tokens, in
   addition to ending their sessions as before. Scripts that use an access token of that user need a
   new one.

-  Proxy: Pages that tasks serve under ``/proxy/``, such as notebooks, TensorBoards, and ports
   opened with ``proxy_ports``, share the master's origin. The checks above cannot tell them apart
   from the WebUI: their scripts can send requests with the session of any user who opens them, with
   that user's permissions. Open task services only from users you trust, especially as an
   administrator. Serving ``/proxy/`` from a separate origin, and limiting proxied services to their
   owner, are not part of this release. The master also does not ask administrators for their
   password again before actions that grant more access, such as creating users or administrators,
   making a user an administrator, setting another user's password, or creating access tokens.

**Breaking Changes**

-  CLI, Python SDK: Older versions of ``det user change-password``, ``det user edit --username``,
   and ``det user rename``, and of ``User.change_password`` and ``User.rename`` in the Python SDK,
   fail when users change their own password or username, because they do not send the current
   password. Upgrade the CLI and the SDK, which ask for it, or use the WebUI. Administrators
   changing or renaming other users are not affected.

-  API: ``POST /api/v1/users/{user_id}/password`` now takes the whole request as its body,
   ``{"password": "...", "old_password": "..."}``, instead of a bare JSON string with the new
   password. The WebUI is updated; other clients that send a bare string get ``400 Bad Request``.

-  WebUI: In **Admin > Users**, administrators editing their own user can no longer set its password
   there. Use **Change Password** in the user settings, which asks for the current password.

**Bug Fixes**

-  API: Editing a user, for example their display name, and the user updates of single sign-on no
   longer revoke the user's access tokens. Only deactivating the user or setting a new password
   does.

-  API: A token whose session has ended, for example after signing out or a password change, an
   access token that a new password revoked, and an expired single sign-on session now get ``401
   Unauthorized`` on the legacy routes, and a redirect to the sign-in page under ``/proxy/``,
   instead of ``500 Internal Server Error``.
