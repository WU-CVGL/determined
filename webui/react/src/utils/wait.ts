import { paths } from 'routes/utils';
import { V1LaunchWarning } from 'services/api-ts-sdk';
import {
  Command,
  CommandResponse,
  CommandState,
  CommandTask,
  CommandType,
  InteractiveTaskTab,
} from 'types';
import { openBlank } from 'utils/routes';

export interface WaitStatus {
  isReady: boolean;
  state: CommandState;
}

/** Opens the task page of a task in its own browser tab, named after the task, on a tab of it. */
export const openInteractiveTask = (
  taskId: string,
  params: { currentSlotsExceeded?: boolean; tab?: InteractiveTaskTab } = {},
): void => {
  openBlank(`${process.env.PUBLIC_URL}${paths.interactiveTask(taskId, params)}`, taskId);
};

export const openCommand = (command: Pick<CommandTask, 'id'>): void => {
  openInteractiveTask(command.id);
};

export const openCommandResponse = (commandResponse: CommandResponse): void => {
  const warnings = commandResponse?.warnings ? commandResponse.warnings : [];
  openInteractiveTask(commandResponse.command.id, {
    currentSlotsExceeded: warnings.includes(V1LaunchWarning.CURRENTSLOTSEXCEEDED),
  });
};

export const CANNOT_OPEN_COMMAND_ERROR = 'Command cannot be opened.';

const openableCommands: Set<string> = new Set([CommandType.TensorBoard]);
export const waitPageUrl = (command: Command | CommandTask): string => {
  if (!openableCommands.has(command.type) || !command.serviceAddress)
    throw new Error(CANNOT_OPEN_COMMAND_ERROR);

  const type = command.type;
  const waitPath = `${process.env.PUBLIC_URL}/wait/${type.toLowerCase()}/${command.id}`;
  const waitParams = `?serviceAddr=${command.serviceAddress}`;
  return waitPath + waitParams;
};
