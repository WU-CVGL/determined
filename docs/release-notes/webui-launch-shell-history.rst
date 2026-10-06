:orphan:

**New Features**

-  WebUI: Launch shells from the browser. The task list (the Tasks page and a workspace's Tasks tab)
   and the Home page have a **Launch Shell** button next to **Launch JupyterLab**. Both open one
   launch form with that task type selected; a **JupyterLab** | **Shell** selector at the top of the
   form switches the type and keeps everything entered. The rest of the form (workspace, start from,
   name, resource pool, slots, the full-config YAML mode and **Save as Template**) is the same for
   both types. After a shell starts, the WebUI offers **Open Terminal** for a terminal in the
   browser, shows the ``det shell open <id>`` command to connect with, and links to the shell's logs
   and resources. The shell's SSH private key, which the launch API returns for the CLI, is
   discarded by the WebUI as soon as the response arrives and is never stored.

-  WebUI: Start a shell or JupyterLab from an earlier config. The launch form's **Start from**
   picker (formerly **Template**) offers the items below. Recent tasks and this browser's history
   list both shells and JupyterLabs, each labelled with its type, since both use the same config
   format: picking one fills the form and keeps the selected task type.

   -  **Recent on cluster**: your own shells and JupyterLabs that the master still knows about, that
      is, running ones and ones that ended in about the last 24 hours. The master forgets ended
      tasks after that and when it restarts.

   -  **Recently launched in this browser**: up to 20 configs per task type that you launched from
      this browser. They are kept in the browser's local storage under your user ID. Only the launch
      config is stored, plus its workspace and time, without the entrypoint, registry credentials or
      environment variables whose names look like credentials (``TOKEN``, ``SECRET``, ``PASSW``,
      ``KEY``, ``AUTH``, ``CRED``). This covers ``environment_variables`` and the ``env`` entries
      with a ``value`` of the containers and init containers in a Kubernetes ``pod_spec``. The list
      is a convenience only and can be cleared from the picker.

   -  **Templates**: picking a template now also fills in its resource pool and slots, which the
      master does not apply from a template for shells and JupyterLabs. The template of your last
      launch is preselected, with your last slots.

-  WebUI: **Launch Again** on your own shell and JupyterLab rows in the task list and on the
   resource pool page opens the launch form with the task's type selected, filled from that task's
   config. Administrators see it on every shell and JupyterLab.

-  WebUI: **Save as Template** in the full-config mode of the launch form starts a new template from
   the config, keeping only the settings that differ from the cluster defaults. It appears only when
   templates are enabled and you may create templates in the selected workspace. Other users can
   read templates, so review the config before saving it.

-  WebUI: The full config of a new shell is previewed through the JupyterLab preview, because the
   shell launch API has no preview. The JupyterLab settings ``idle_timeout`` and
   ``notebook_idle_type`` stay in it: a shell ignores them, and switching the form back to
   JupyterLab launches with them.
