import { paths } from 'routes/utils';
import { V1LaunchWarning } from 'services/api-ts-sdk';
import { CommandResponse, CommandTask, InteractiveTaskTab } from 'types';
import { openBlank } from 'utils/routes';

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
