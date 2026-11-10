:orphan:

**Bug Fixes**

-  API: Adding or removing one experiment label (``PUT`` and ``DELETE``
   ``/api/v1/experiments/{experiment_id}/labels/{label}``) failed for a label with a slash, such as
   ``train/validation``, which the CLI uses when the experiment's title is not shared with the
   user.
