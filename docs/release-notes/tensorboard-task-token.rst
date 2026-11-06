:orphan:

**Security Fixes**

-  TensorBoard: The TensorBoard task's TensorBoard refuses requests without the master proxy's
   token, in every network mode, so that only the proxy reaches its data. The task needs TensorBoard
   2.9 or later in its image. TensorBoard tasks that are running or queued at the upgrade keep their
   earlier TensorBoard, without the token, until they end: kill them and start them again after the
   upgrade. Their event data is untouched.
