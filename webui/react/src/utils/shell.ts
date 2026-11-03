import { Loadable } from 'hew/utils/loadable';

import { launchShell as apiLaunchShell, previewShell as apiPreviewShell } from 'services/api';
import userStore from 'stores/users';
import { CommandResponse, RawJson } from 'types';
import { recordLaunch } from 'utils/launchHistory';
import { configForShellLaunch, NtscLaunchOptions, simpleLaunchConfig } from 'utils/ntscConfig';

export interface ShellLaunchOptions extends NtscLaunchOptions {
  /** A full config. When given, name, pool, slots and template are ignored. */
  config?: RawJson;
}

/**
 * Launches a shell and returns the new task. The services wrapper has already
 * dropped the shell's SSH key from the response, so nothing here can keep it.
 * Errors are thrown so the launch modal can stay open and show them.
 *
 * A shell cannot be opened like a TensorBoard (utils/wait); callers show how to
 * connect instead.
 */
export const launchShell = async (options: ShellLaunchOptions = {}): Promise<CommandResponse> => {
  const response = await apiLaunchShell({
    config: options.config ? configForShellLaunch(options.config) : simpleLaunchConfig(options),
    templateName: options.config || options.template === '' ? undefined : options.template,
    workspaceId: options.workspaceId,
  });
  const currentUser = Loadable.getOrElse(undefined, userStore.currentUser.get());
  recordLaunch(currentUser?.id, {
    config: response.config,
    workspaceId: response.command.workspaceId,
  });
  return response;
};

/**
 * Previews the merged config of a shell launch with these options, through the master's shell
 * preview. No task is created.
 */
export const previewShell = async (options: ShellLaunchOptions = {}): Promise<RawJson> => {
  try {
    return await apiPreviewShell({
      config: options.config ? configForShellLaunch(options.config) : simpleLaunchConfig(options),
      templateName: options.config || options.template === '' ? undefined : options.template,
      workspaceId: options.workspaceId,
    });
  } catch (e) {
    throw new Error('Unable to load shell config.');
  }
};
