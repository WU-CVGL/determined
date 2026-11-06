:orphan:

**Bug Fixes**

-  TensorBoard: The TensorBoard task resumes fetching event files after a short storage outage. When
   every download failed at once, the task could stop fetching new files for good.
