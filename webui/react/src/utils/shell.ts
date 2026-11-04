import { Loadable } from 'hew/utils/loadable';

import { launchShell as apiLaunchShell, previewShell as apiPreviewShell } from 'services/api';
import { LaunchShellParams } from 'services/types';
import userStore from 'stores/users';
import { CommandResponse, RawJson } from 'types';
import { isDetError } from 'utils/error';
import { recordLaunch } from 'utils/launchHistory';
import { NtscLaunchOptions, simpleLaunchConfig } from 'utils/ntscConfig';
import { getResponseStatus } from 'utils/service';

export interface ShellLaunchOptions extends NtscLaunchOptions {
  /** A full config. When given, name, pool, slots, services and template are ignored. */
  config?: RawJson;
}

/** The request a launch and a preview send for these options: the same for both. */
export const shellRequest = (options: ShellLaunchOptions): LaunchShellParams => ({
  config: options.config ?? simpleLaunchConfig(options),
  templateName: options.config || options.template === '' ? undefined : options.template,
  workspaceId: options.workspaceId,
});

/** The statuses of the master's refusals that the user can act on: InvalidArgument, PermissionDenied. */
const ACTIONABLE_STATUSES = [400, 403];

/**
 * The master's message for a refused request that the user can act on, such as an unknown service
 * or a resource pool the user may not use; undefined for any other error.
 */
export const actionableErrorMessage = (e: unknown): string | undefined => {
  if (!isDetError(e)) return undefined;
  const status = getResponseStatus(e.sourceErr);
  if (status === undefined || !ACTIONABLE_STATUSES.includes(status)) return undefined;
  return e.publicMessage || undefined;
};

export const PREVIEW_ERROR = 'Unable to fetch shell config.';

/**
 * Launches a shell, records its config in this browser's launch history and
 * returns the new task. The services wrapper has already dropped the shell's
 * SSH key from the response, so nothing here can keep it. Errors are thrown so
 * the launch modal can stay open and show them.
 *
 * Nothing here waits for the shell to run or opens it: the caller shows the
 * launched shell, whose task page opens by the task's ID.
 */
export const launchShell = async (options: ShellLaunchOptions = {}): Promise<CommandResponse> => {
  const response = await apiLaunchShell(shellRequest(options));
  const currentUser = Loadable.getOrElse(undefined, userStore.currentUser.get());
  recordLaunch(currentUser?.id, {
    config: response.config,
    workspaceId: response.command.workspaceId,
  });
  return response;
};

/**
 * Previews the merged config of a shell launch with these options, through the master's shell
 * preview, which checks it as a launch does. No task is created. A refusal the user can act on
 * is thrown with the master's message, anything else with PREVIEW_ERROR.
 */
export const previewShell = async (options: ShellLaunchOptions = {}): Promise<RawJson> => {
  try {
    return await apiPreviewShell(shellRequest(options));
  } catch (e) {
    throw new Error(actionableErrorMessage(e) ?? PREVIEW_ERROR);
  }
};
