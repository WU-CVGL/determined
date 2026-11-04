import { MaybeMocked } from '@vitest/spy';

import { CommandState, CommandTask, CommandType, RecordKey } from 'types';
import { generateUUID } from 'utils/string';

import * as utils from './wait';

const UUID = {
  [CommandType.JupyterLab]: generateUUID(),
  [CommandType.Shell]: generateUUID(),
  [CommandType.TensorBoard]: generateUUID(),
};
const UUID_REGEX = '[a-z\\d-]+';
const SHARED_TASK: Partial<CommandTask> = {
  misc: undefined,
  url: undefined,
};
const COMMAND_TASK: Record<RecordKey, CommandTask> = {
  [CommandType.JupyterLab]: {
    ...SHARED_TASK,
    displayName: 'lee sung hoon',
    id: UUID[CommandType.JupyterLab],
    name: 'JupyterLab (directly-sharp-penguin)',
    resourcePool: 'compute-pool',
    serviceAddress: `/proxy/${UUID[CommandType.JupyterLab]}`,
    startTime: '2022-08-01T00:12:24Z',
    state: CommandState.Queued,
    type: CommandType.JupyterLab,
    userId: 34,
    workspaceId: 0,
  },
  [CommandType.Shell]: {
    ...SHARED_TASK,
    displayName: 'lee sung hoon',
    id: UUID[CommandType.Shell],
    name: 'Shell (jolly-well-pheasant)',
    resourcePool: 'compute-pool',
    serviceAddress: `/proxy/${UUID[CommandType.Shell]}`,
    startTime: '2022-07-29T24:00:12Z',
    state: CommandState.Terminated,
    type: CommandType.Shell,
    userId: 34,
    workspaceId: 0,
  },
  [CommandType.TensorBoard]: {
    ...SHARED_TASK,
    displayName: 'racer kim',
    id: UUID[CommandType.TensorBoard],
    name: 'TensorBoard (ambiguously-happy-ear)',
    resourcePool: 'aux-pool',
    serviceAddress: `/proxy/${UUID[CommandType.TensorBoard]}`,
    startTime: '2022-08-02T12:24:00Z',
    state: CommandState.Running,
    type: CommandType.TensorBoard,
    userId: 16,
    workspaceId: 0,
  },
};

describe('Wait Page Utilities', () => {
  describe('openCommand', () => {
    let windowOpen: MaybeMocked<typeof global.open>;

    beforeEach(() => {
      vi.spyOn(global, 'open');
      windowOpen = vi.mocked(global.open);
      windowOpen.mockReset();
    });

    afterEach(() => {
      // Restore `global.open` to original function.
      vi.mocked(global.open).mockRestore();
    });

    it('should open window for TensorBoard task', () => {
      expect(windowOpen).not.toHaveBeenCalled();
      utils.openCommand(COMMAND_TASK[CommandType.TensorBoard]);
      // TODO: Expand this to use `toHaveBeenCalledWith`.
      expect(windowOpen).toHaveBeenCalled();
    });

    it.each([CommandType.JupyterLab, CommandType.Shell])(
      'should throw error for a %s task, which is not a TensorBoard',
      (type) => {
        expect(() => {
          utils.openCommand(COMMAND_TASK[type]);
        }).toThrow(utils.CANNOT_OPEN_COMMAND_ERROR);
      },
    );
  });

  describe('waitPageUrl', () => {
    it('should convert task to wait page url', () => {
      expect(utils.waitPageUrl(COMMAND_TASK[CommandType.TensorBoard])).toMatch(
        new RegExp(`wait/${CommandType.TensorBoard}/${UUID_REGEX}`, 'i'),
      );
    });

    it.each([CommandType.JupyterLab, CommandType.Shell])(
      'should throw error for a %s task, which is not a TensorBoard',
      (type) => {
        expect(() => {
          utils.waitPageUrl(COMMAND_TASK[type]);
        }).toThrow(utils.CANNOT_OPEN_COMMAND_ERROR);
      },
    );
  });
});
