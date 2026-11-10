import argparse
import operator
import os
import pathlib
import typing
import urllib.parse
import webbrowser

import termcolor

from determined import cli
from determined.cli import ntsc, render, task
from determined.common import api
from determined.common.api import bindings

# The idle timeout of a viewing session whose configuration sets none.
VIEWING_SESSION_IDLE_TIMEOUT = "5m"


def viewing_session_config(
    config: typing.Dict[str, typing.Any],
    experiment_ids: typing.List[int],
    trial_ids: typing.Optional[typing.List[int]],
) -> typing.Dict[str, typing.Any]:
    """
    Returns the shell configuration of a viewing session: config with a tensorboard service that
    reads the experiments and trials, without SSH, and with an idle timeout. A services or
    services.tensorboard entry that is not a map leaves config unchanged, for the master to refuse.
    """
    services = config.get("services") or {}
    if not isinstance(services, dict):
        return config
    tb = services.get("tensorboard") or {}
    if not isinstance(tb, dict):
        return config
    if "tensorboard_args" in config:
        legacy = config.pop("tensorboard_args")
        tb.setdefault("args", [legacy] if isinstance(legacy, str) else legacy)
    if experiment_ids:
        tb["experiment_ids"] = experiment_ids
    if trial_ids:
        tb["trial_ids"] = trial_ids
    services["tensorboard"] = tb
    ssh = services.get("ssh")
    services["ssh"] = {**(ssh if isinstance(ssh, dict) else {}), "enabled": False}
    config["services"] = services
    config.setdefault("idle_timeout", VIEWING_SESSION_IDLE_TIMEOUT)
    return config


# The kinds of shell: a viewing session, which the tensorboard commands manage, and another shell,
# which they refuse.
VIEWING_SESSION = "session"
SHELL = "shell"


def viewing_session_url(master: str, task_id: str) -> str:
    """Returns the WebUI page of a viewing session's TensorBoard."""
    public_url = os.environ.get("PUBLIC_URL", "/det")
    task_id = urllib.parse.quote(task_id, safe="")
    return f"{master.rstrip('/')}{public_url}/tasks/{task_id}/interactive?tab=tensorboard"


def primary_service(sess: api.Session, task_id: str) -> str:
    """
    Returns the primary service of the task task_id: tensorboard for a viewing session, ssh for any
    other shell, and none for another task or a task that is not found. The task's services, unlike
    GetShell, hold no credential, and the master answers them for ended tasks as well.
    """
    try:
        return bindings.get_GetTaskServices(sess, taskId=task_id).primary
    except api.errors.NotFoundException:
        return ""


def shell_kind(sess: api.Session, shell: typing.Dict[str, typing.Any]) -> str:
    """Returns whether a shell of api/v1/shells is a viewing session or another shell."""
    # The master runs a shell without ssh only with resources.slots 0.
    if shell.get("slots"):
        return SHELL
    return VIEWING_SESSION if primary_service(sess, shell["id"]) == "tensorboard" else SHELL


def resolve_ids(sess: api.Session, prefixes: typing.List[str], subcommand: str) -> typing.List[str]:
    """
    Returns the ID of the viewing session that each ID or ID prefix names, among the shells the
    user may see. A shell that is not a viewing session is refused, and so is a full ID that names
    no shell.
    """
    shells: typing.List[typing.Dict[str, typing.Any]] = []
    if not all(ntsc.TASK_ID_REGEX.match(p) for p in prefixes):
        shells = sess.get("api/v1/shells").json()["shells"]

    resolved = []
    for prefix in prefixes:
        if ntsc.TASK_ID_REGEX.match(prefix):
            primary = primary_service(sess, prefix)
            if not primary:
                raise cli.errors.CliError(f"tensorboard {prefix} not found")
            resolved.append((prefix, VIEWING_SESSION if primary == "tensorboard" else SHELL))
            continue
        others = [(x["id"], shell_kind(sess, x)) for x in shells if x["id"].startswith(prefix)]
        found = [x for x in others if x[1] == VIEWING_SESSION]
        if not found and len(others) == 1:
            found = others
        if len(found) > 1:
            ids = ", ".join(x[0] for x in found)
            raise api.errors.BadRequestException(f"partial UUID '{prefix}' not unique: {ids}")
        elif not found:
            raise api.errors.BadRequestException(f"partial UUID '{prefix}' not found")
        resolved.append(found[0])
    for task_id, kind in resolved:
        if kind == SHELL:
            raise cli.errors.CliError(
                f"{task_id} is a shell, not a TensorBoard; use det shell {subcommand}"
            )
    return [task_id for task_id, _ in resolved]


def start_tensorboard(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    if not (args.trial_ids or args.experiment_ids):
        raise argparse.ArgumentError(None, "Either experiment_ids or trial_ids must be specified.")

    config = ntsc.parse_config(args.config_file, None, args.config, [])
    config = viewing_session_config(config, args.experiment_ids, args.trial_ids)

    workspace_id = cli.workspace.get_workspace_id_from_args(args)

    resp = bindings.v1LaunchShellResponse.from_json(
        ntsc.launch_command(
            sess,
            "api/v1/shells",
            config,
            "",
            context_path=args.context,
            includes=args.include,
            workspace_id=workspace_id,
        )
    )
    shell = resp.shell

    if args.detach:
        print(shell.id)
        return

    render.report_job_launched("tensorboard", shell.id, shell.description)

    if resp.warnings:
        cli.print_launch_warnings(resp.warnings)
    loading = render.Animator("Waiting for tensorboard to become ready")
    err = api.wait_for_task_ready(
        session=sess, task_id=shell.id, progress_report=loading.next, timeout=60 * 30
    )
    loading.clear(
        f"tensorboard (id: {shell.id}) is ready." if not err else f"Waiting stopped: {err}"
    )
    if err:
        raise cli.errors.CliError(err)

    url = viewing_session_url(args.master, shell.id)
    if not args.no_browser:
        webbrowser.open(url)
    print(termcolor.colored(f"Tensorboard is running at: {url}", "green"))


def open_tensorboard(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    tensorboard_id = resolve_ids(sess, [args.tensorboard_id], "open")[0]

    task = bindings.get_GetTask(sess, taskId=tensorboard_id).task
    if task.endTime is not None:
        raise cli.errors.CliError("Tensorboard has ended")
    url = viewing_session_url(args.master, tensorboard_id)
    webbrowser.open(url)
    print(url)


def list_tensorboards(args: argparse.Namespace) -> None:
    """
    Lists the viewing sessions as det shell list lists shells; --json prints them as api/v1/shells
    returns them.
    """
    sess = cli.setup_session(args)
    table_header = ntsc.TensorboardTableHeader

    params: typing.Dict[str, typing.Any] = {}
    if "workspace_name" in args and args.workspace_name is not None:
        params["workspaceId"] = api.workspace_by_name(sess, args.workspace_name).id
    if not args.all:
        params["users"] = [sess.username]

    shells = sess.get("api/v1/shells", params=params).json()["shells"]
    res = [x for x in shells if shell_kind(sess, x) == VIEWING_SESSION]

    if args.quiet:
        for item in res:
            print(item["id"])
        return

    w_names = cli.workspace.get_workspace_names(sess)
    for item in res:
        if item["state"].startswith("STATE_"):
            item["state"] = item["state"][6:]
        if "workspaceId" in item:
            wId = item["workspaceId"]
            item["workspaceName"] = (
                w_names[wId] if wId in w_names else f"missing workspace id {wId}"
            )

    res.sort(key=operator.itemgetter(args.sort_by), reverse=args.order_by == "desc")

    if getattr(args, "json", None):
        render.print_json(res)
        return

    values = render.select_values(res, table_header)
    render.tabulate_or_csv(table_header, values, getattr(args, "csv", False))


def kill_tensorboards(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    targets = resolve_ids(sess, args.tensorboard_id, "kill")

    for i, task_id in enumerate(targets):
        try:
            ntsc._kill(sess, ntsc.TaskTypeShell, task_id)
            print(termcolor.colored(f"Killed tensorboard {task_id}", "green"))
        except api.errors.APIException as e:
            if not args.force:
                for ignored in targets[i + 1 :]:
                    print(f"Cowardly not killing {ignored}")
                raise e
            print(termcolor.colored(f"Skipping: {e} ({type(e).__name__})", "red"))


def tensorboard_config(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    task_id = resolve_ids(sess, [args.tensorboard_id], "config")[0]
    res_json = sess.get(f"api/v1/shells/{task_id}").json()
    print(render.format_object_as_yaml(res_json["config"]))


def tensorboard_logs(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    args.task_id = resolve_ids(sess, [args.task_id], "logs")[0]
    task.logs(args)


def set_tensorboard_priority(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    task_id = resolve_ids(sess, [args.tensorboard_id], "set priority")[0]
    try:
        sess.post(f"api/v1/shells/{task_id}/set_priority", json={"priority": args.priority})
        print(
            termcolor.colored(f"Set priority of tensorboard {task_id} to {args.priority}", "green")
        )
    except api.errors.APIException as e:
        print(termcolor.colored(f"Skipping: {e} ({type(e).__name__})", "red"))


args_description: cli.ArgsDescription = [
    cli.Cmd(
        "tensorboard",
        None,
        "manage TensorBoard instances",
        [
            cli.Cmd(
                "list ls",
                list_tensorboards,
                "list TensorBoard instances",
                ntsc.ls_sort_args
                + [
                    cli.Arg("-q", "--quiet", action="store_true", help="only display the IDs"),
                    cli.Arg(
                        "--all",
                        "-a",
                        action="store_true",
                        help="show all TensorBoards (including other users')",
                    ),
                    cli.workspace.workspace_arg,
                    cli.Group(cli.output_format_args["json"], cli.output_format_args["csv"]),
                ],
                is_default=True,
            ),
            cli.Cmd(
                "start",
                start_tensorboard,
                "start new TensorBoard instance",
                [
                    cli.Arg(
                        "experiment_ids",
                        type=int,
                        nargs="*",
                        help="experiment IDs to load into TensorBoard",
                    ),
                    cli.Arg(
                        "-t",
                        "--trial-ids",
                        nargs=argparse.ONE_OR_MORE,
                        type=int,
                        help="trial IDs to load into TensorBoard",
                    ),
                    cli.workspace.workspace_arg,
                    cli.Arg(
                        "--config-file",
                        default=None,
                        type=argparse.FileType("r"),
                        help="command config file (.yaml)",
                    ),
                    cli.Arg(
                        "-c", "--context", default=None, type=pathlib.Path, help=ntsc.CONTEXT_DESC
                    ),
                    cli.Arg(
                        "-i",
                        "--include",
                        default=[],
                        action="append",
                        type=pathlib.Path,
                        help=ntsc.INCLUDE_DESC,
                    ),
                    cli.Arg("--config", action="append", default=[], help=ntsc.CONFIG_DESC),
                    cli.Arg(
                        "--no-browser",
                        action="store_true",
                        help="don't open TensorBoard in a browser after startup",
                    ),
                    cli.Arg(
                        "-d",
                        "--detach",
                        action="store_true",
                        help="run in the background and print the ID",
                    ),
                ],
            ),
            cli.Cmd(
                "config",
                tensorboard_config,
                "display TensorBoard config",
                [cli.Arg("tensorboard_id", type=str, help="TensorBoard ID")],
            ),
            cli.Cmd(
                "open",
                open_tensorboard,
                "open existing TensorBoard instance",
                [cli.Arg("tensorboard_id", help="TensorBoard ID")],
            ),
            cli.Cmd(
                "logs",
                tensorboard_logs,
                "fetch TensorBoard instance logs",
                [
                    cli.Arg("task_id", help="TensorBoard ID", metavar="tensorboard_id"),
                    *task.common_log_options,
                ],
            ),
            cli.Cmd(
                "kill",
                kill_tensorboards,
                "kill TensorBoard instance",
                [
                    cli.Arg("tensorboard_id", help="TensorBoard ID", nargs=argparse.ONE_OR_MORE),
                    cli.Arg("-f", "--force", action="store_true", help="ignore errors"),
                ],
            ),
            cli.Cmd(
                "set",
                None,
                "set TensorBoard attributes",
                [
                    cli.Cmd(
                        "priority",
                        set_tensorboard_priority,
                        "set TensorBoard priority",
                        [
                            cli.Arg("tensorboard_id", help="TensorBoard ID"),
                            cli.Arg("priority", type=int, help="priority"),
                        ],
                    ),
                ],
            ),
        ],
    )
]
