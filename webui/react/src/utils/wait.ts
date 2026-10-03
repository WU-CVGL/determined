import { paths } from 'routes/utils';
import { getJupyterLab } from 'services/api';
import { V1LaunchWarning } from 'services/api-ts-sdk';
import { Command, CommandResponse, CommandState, CommandTask, CommandType } from 'types';
import { openBlank } from 'utils/routes';
import { isCommandTask } from 'utils/task';

export interface WaitStatus {
  isReady: boolean;
  state: CommandState;
}

export const openCommand = (command: CommandTask): void => {
  openBlank(`${process.env.PUBLIC_URL}${paths.interactive(command, false)}`, command.id);
};

export const openCommandResponse = (commandResponse: CommandResponse): void => {
  const warnings = commandResponse?.warnings ? commandResponse.warnings : [];
  const maxSlotsExceeded = warnings.includes(V1LaunchWarning.CURRENTSLOTSEXCEEDED);
  openBlank(
    `${process.env.PUBLIC_URL}${paths.interactive(commandResponse.command, maxSlotsExceeded)}`,
    commandResponse.command.id,
  );
};

export const CANNOT_OPEN_COMMAND_ERROR = 'Command cannot be opened.';

const openableCommands: Set<string> = new Set([CommandType.JupyterLab, CommandType.TensorBoard]);
export const waitPageUrl = (command: Command | CommandTask): string => {
  if (!openableCommands.has(command.type) || !command.serviceAddress)
    throw new Error(CANNOT_OPEN_COMMAND_ERROR);

  const type = isCommandTask(command) ? command.type : command.type;
  const waitPath = `${process.env.PUBLIC_URL}/wait/${type.toLowerCase()}/${command.id}`;
  const waitParams = `?serviceAddr=${command.serviceAddress}`;
  return waitPath + waitParams;
};

export const NOTEBOOK_ACCESS_DENIED =
  'Only the user who started this notebook or an administrator can open it.';

// The master puts a notebook's Jupyter token in its service address only when the current user
// started the notebook or is an administrator, and never in listings.
export const hasJupyterToken = (serviceAddress?: string | null): boolean =>
  !!serviceAddress && /[?&]token=[^&]+/.test(serviceAddress);

// Returns the address that opens a notebook in JupyterLab, or undefined if the current user may not
// open it.
export const getJupyterLabAddress = async (notebookId: string): Promise<string | undefined> => {
  const notebook = await getJupyterLab({ commandId: notebookId });
  return hasJupyterToken(notebook.serviceAddress) ? notebook.serviceAddress : undefined;
};
