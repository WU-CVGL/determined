:orphan:

**Security Fixes**

-  TensorBoard: The TensorBoard task's TensorBoard refuses requests without the master proxy's
   token, in every network mode, so that only the proxy reaches its data. The task needs TensorBoard
   2.9 or later in its image.
