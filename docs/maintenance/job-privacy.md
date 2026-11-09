# Job privacy

Job privacy decides who besides a job's owner may read three kinds of a job's
content:

- the **title**: the name, description, labels, and notes;
- the **logs**: the task logs, including startup and system logs, following,
  searching, log fields, and downloads;
- the **config**.

A job's owner and administrators always read all of it. Everything else about a
job is visible to every user who may view it: its existence, ID, type, owner,
workspace, project, pool, slots, queue position, and state. Job privacy only
narrows the existing basic or RBAC checks; it never grants access.

Administrators are the users who may update the master configuration, which
with basic authorization means `admin` is set.

Everything is shared by default.

## The rule

Another user reads a kind of a job's content when all of these hold:

- The cluster policy allows sharing the kind.
- The owner shares it: the owner's switch for titles or for logs is on. There
  is no switch for configs.
- The job's own audience for the kind is all users.
- For the logs and the config, the title is readable too, because system log
  lines and the config carry the job's name.

The policy and the switches apply to every job when it is read, so turning one
off hides the content of the jobs created before as well. A job's own audiences
are set when the job is created, from the owner's defaults or else the cluster's
defaults, and change only when the owner or an administrator changes them.

A kind that the policy forbids when a job is created gets the audience of the
owner and administrators, so allowing it later does not share that job. A job
created while the kind was allowed keeps its audience, so forbidding the kind
and allowing it again shares that job again.

A job created before job privacy has no audiences of its own and is shared
under the policy and its owner's switches.

The trials of an experiment follow the experiment. Deleting checkpoints creates
a checkpoint garbage collection job owned by the user who deletes them. A task
recorded without a job follows the record of its command, shell, or generic
task, or the experiment of its trial, as a job created before job privacy; a
task without such a record is readable only by administrators.

## Settings

- **Policy**: administrators allow or forbid sharing of titles, logs, and
  configs, and set each kind's audience for new jobs: all users, or the owner
  and administrators. Every user may read the policy.
- **User switches**: each user turns sharing of their titles and of their logs
  on or off, and may set the audience of titles and of logs for their new jobs;
  without one, new jobs take the cluster's default. Setting all users for a kind
  that the policy forbids is refused.
- **Job sharing**: a job's owner and administrators set the job's title, logs,
  and config audiences. Setting all users for a kind that the policy forbids is
  refused. The job records who changed its sharing and when.

## What readers see

- A hidden title is left out: the name, description, labels, and notes are
  empty. Searches and filters match such a job only by ID, sorting by name or
  description does not depend on its title, and its labels are left out of the
  label list.
- Hidden logs are refused with `logs not shared`. Free-text failure reasons
  follow the logs, because they carry startup diagnostics that are also in the
  task log: the exit reasons of allocations, the exit statuses of commands and
  shells, and the error messages of resource failures are empty. States,
  failure types, exit codes, resources, and times stay.
- A hidden config is left out of a job's details, and a generic task's config
  is refused with `config not shared`, also as the parent of a fork. Experiment
  lists keep only `resources.slots_per_trial` and `resources.resource_pool` of
  a hidden config. A checkpoint's experiment config keeps only
  `checkpoint_storage`.
- Experiments, runs, commands, shells, generic tasks, jobs in the queue, and
  tasks carry `capabilities` in lists and details: `viewTitle`, `viewLogs`,
  `viewConfig`, and `manageSharing`.

Metrics, hyperparameters, TensorBoard sources, and checkpoint files are not
covered and stay visible to every user who may view the job.

## Changes

A change applies to the next request. An open log stream ends at its next
permission check, within five minutes.

## Not covered

- **Config in logs**: logs can contain parts of the config, such as startup
  traces. Shared logs with a hidden config show them.
- **Code**: model definitions, context directories, and the copy of the model
  definition made by forking an experiment follow the permission to read
  experiment artifacts, not the config. The model definition download still
  contains the experiment's configuration file when the context directory has
  it.
- **Master logs**: they carry job names.
- **Prometheus**: `/prom/det-state-metrics` exports task names in `task_actor`
  of `det_allocation_id_task_id_task_actor` and experiment labels in
  `det_experiment_id_label`.
- **Usage**: the aggregated usage by experiment label counts the labels of every
  experiment.
- **Forbid, then allow**: jobs created before a forbid are shared again when the
  kind is allowed again (see "The rule").
- **Tasks without a job**: tasks recorded without a job and without a command,
  shell, generic task, or trial record are readable only by administrators.

## REST API

| Method and path                         | Who                     |
|-----------------------------------------|-------------------------|
| `GET /api/v1/job-privacy/policy`        | every user              |
| `PUT /api/v1/job-privacy/policy`        | administrators          |
| `GET /api/v1/me/job-privacy`            | every user, their own   |
| `PATCH /api/v1/me/job-privacy`          | every user, their own   |
| `GET /api/v1/jobs/{job_id}/sharing`     | owner and administrators |
| `PATCH /api/v1/jobs/{job_id}/sharing`   | owner and administrators |

An audience is `{"allUsers": true}` for all users and `{"allUsers": false}` for
the owner and administrators. A policy update names every kind once:

```json
{
  "rules": [
    {"kind": "JOB_CONTENT_KIND_TITLE", "sharingAllowed": true,
     "newJobAudience": {"allUsers": true}},
    {"kind": "JOB_CONTENT_KIND_LOGS", "sharingAllowed": true,
     "newJobAudience": {"allUsers": false}},
    {"kind": "JOB_CONTENT_KIND_CONFIG", "sharingAllowed": false,
     "newJobAudience": {"allUsers": false}}
  ]
}
```

A user update sets `shareTitles`, `shareLogs`, `newJobTitles`, and
`newJobLogs`, or `resetNewJobTitles` and `resetNewJobLogs` to return to the
cluster's default. A job sharing update sets `title`, `logs`, and `config`.
Fields left out stay as they are.

## Error messages

- `PermissionDenied` (`403`): `logs not shared`, `config not shared`,
  `only admins set the job privacy policy`, and
  `only the job's owner and admins manage its sharing`.
- `FailedPrecondition` (`400`): `sharing of <kind> is forbidden`, where
  `<kind>` is `title`, `logs`, or `config`.
- `InvalidArgument` (`400`): a policy update without exactly one rule per kind
  or with a rule that has no `newJobAudience`, or a user update that both sets
  and resets the same default.
- `NotFound` (`404`): an unknown job.
- `Unavailable` (`503`): `could not read the job privacy policy: <error>; try
  again`, `could not read job privacy preferences: <error>; try again`, and
  `could not read job privacy: <error>; try again`. Job privacy is never
  taken as shared when it cannot be read.

## Backup and rollback

The policy is stored in `job_privacy_policy`, the user switches in
`user_job_privacy`, and the job audiences in the `jobs` table. If the policy
row of a kind is missing, sharing of that kind is forbidden. Rolling back to a
master without job privacy shares everything again; the database needs no
restore.
