import argparse
import functools
import os
import pathlib
import typing
import urllib.parse
import webbrowser

import termcolor

from determined import cli
from determined.cli import ntsc, render, task
from determined.common import api, check
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
    cli.wait_ntsc_ready(sess, api.NTSC_Kind.tensorboard, shell.id)

    public_url = os.environ.get("PUBLIC_URL", "/det")
    task_id = urllib.parse.quote(shell.id, safe="")
    url = f"{args.master.rstrip('/')}{public_url}/tasks/{task_id}/interactive?tab=tensorboard"
    if not args.no_browser:
        webbrowser.open(url)
    print(termcolor.colored(f"Tensorboard is running at: {url}", "green"))


def open_tensorboard(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    tensorboard_id = typing.cast(str, ntsc.expand_uuid_prefixes(sess, args))

    task = bindings.get_GetTask(sess, taskId=tensorboard_id).task
    check.check_none(task.endTime, "Tensorboard has ended")

    tsb = bindings.get_GetTensorboard(sess, tensorboardId=tensorboard_id).tensorboard
    assert tsb.serviceAddress is not None, "missing tensorboard serviceAddress"
    tb_path = ntsc.make_interactive_task_url(
        task_id=tsb.id,
        service_address=tsb.serviceAddress,
        description=tsb.description,
        resource_pool=tsb.resourcePool,
        currentSlotsExceeded=False,
    )
    webbrowser.open(f"{args.master}/{tb_path}")


args_description: cli.ArgsDescription = [
    cli.Cmd(
        "tensorboard",
        None,
        "manage TensorBoard instances",
        [
            cli.Cmd(
                "list ls",
                functools.partial(ntsc.list_tasks),
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
                functools.partial(ntsc.config),
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
                functools.partial(task.logs),
                "fetch TensorBoard instance logs",
                [
                    cli.Arg("task_id", help="TensorBoard ID", metavar="tensorboard_id"),
                    *task.common_log_options,
                ],
            ),
            cli.Cmd(
                "kill",
                functools.partial(ntsc.kill),
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
                        functools.partial(ntsc.set_priority),
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
