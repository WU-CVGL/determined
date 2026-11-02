:orphan:

**Bug Fixes**

-  Master: Stop a proxied WebSocket request at the first error when the TLS connection to the
   task's HTTPS service cannot be set up, instead of writing the request and reporting a second
   error.
