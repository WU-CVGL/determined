:orphan:

**Improvements**

-  Proxy: Log the end of a proxied shell, JupyterLab, TensorBoard or other WebSocket connection at
   debug level instead of as an error when the error only says that the connection ended or dropped:
   a WebSocket close with code 1000, 1001, 1005 or 1006, end of stream, broken pipe, connection
   reset, or a write after a close was sent. This includes abrupt disconnects, such as a browser
   that went away, a network drop or a service that exited, so the default logs no longer show these
   disconnect reasons. Other errors from proxied connections are still logged as errors.
