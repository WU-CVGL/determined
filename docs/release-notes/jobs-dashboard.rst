:orphan:

**New Features**

-  WebUI: Add a Jobs page that lists experiments, generic tasks, notebooks, shells, commands, and
   TensorBoards together, with kind, owner, state, and GPU or CPU-only filters and bulk kill.
   ``/tasks`` shows the same list without experiments.

-  WebUI: Add Jobs tabs to workspaces and to projects, including Uncategorized.

-  API: Filter experiments by workspace, and generic tasks by project, name, and GPU or CPU-only
   use. Notebooks, shells, commands, and TensorBoards report the slots they request.
