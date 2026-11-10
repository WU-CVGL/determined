import { getCommand, getGenericTask, getShells, getTensorBoard } from 'services/api';
import { CommandTask, CommandType, JobCapabilities } from 'types';

/**
 * The record of a command, shell or TensorBoard: its name, pool and, for a TensorBoard, address.
 * None for a task of another type, nor once the master has let go of the record, a day after the
 * task's end.
 */
export const lookupCommandTask = async (
  taskId: string,
  type?: string,
): Promise<CommandTask | undefined> => {
  switch (type) {
    case CommandType.Command:
      return await getCommand({ commandId: taskId });
    case CommandType.TensorBoard:
      return await getTensorBoard({ commandId: taskId });
    case CommandType.Shell:
      // The shell list has no private keys, unlike a shell's own lookup.
      return (await getShells({})).find((shell) => shell.id === taskId);
    default:
      return undefined;
  }
};

/**
 * The capabilities of a task from the record of its type ('generic' for a generic task), for an
 * older master, whose task answer has none. None without a record.
 */
export const lookupLegacyCapabilities = async (
  taskId: string,
  type?: string,
): Promise<JobCapabilities | undefined> =>
  type === 'generic'
    ? (await getGenericTask(taskId))?.capabilities
    : (await lookupCommandTask(taskId, type))?.capabilities;
