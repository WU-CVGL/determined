import { MaybeMocked } from '@vitest/spy';

import { V1LaunchWarning } from 'services/api-ts-sdk';
import { CommandState, CommandTask, CommandType, InteractiveTaskTab, RecordKey } from 'types';
import { generateUUID } from 'utils/string';

import * as utils from './wait';

const UUID = {
  [CommandType.Command]: generateUUID(),
  [CommandType.Shell]: generateUUID(),
};
const SHARED_TASK: Partial<CommandTask> = {
  misc: undefined,
  url: undefined,
};
const COMMAND_TASK: Record<RecordKey, CommandTask> = {
  [CommandType.Command]: {
    ...SHARED_TASK,
    displayName: 'lee sung hoon',
    id: UUID[CommandType.Command],
    name: 'Command (directly-sharp-penguin)',
    resourcePool: 'compute-pool',
    serviceAddress: `/proxy/${UUID[CommandType.Command]}`,
    startTime: '2022-08-01T00:12:24Z',
    state: CommandState.Queued,
    type: CommandType.Command,
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
};

describe('Task Page Openers', () => {
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

    it.each([CommandType.Command, CommandType.Shell])(
      'opens the task page of a %s task in a tab named after the task',
      (type) => {
        const task = COMMAND_TASK[type];
        utils.openCommand(task);
        expect(windowOpen).toHaveBeenCalledWith(
          expect.stringMatching(new RegExp(`/tasks/${task.id}/interactive$`)),
          task.id,
          '',
        );
      },
    );

    it('carries the launch warning about the free slots', () => {
      const task = COMMAND_TASK[CommandType.Shell];
      utils.openCommandResponse({
        command: task,
        warnings: [V1LaunchWarning.CURRENTSLOTSEXCEEDED],
      });
      expect(windowOpen).toHaveBeenCalledWith(
        expect.stringMatching(
          new RegExp(`/tasks/${task.id}/interactive\\?currentSlotsExceeded=true$`),
        ),
        task.id,
        '',
      );
      utils.openCommandResponse({ command: task, warnings: [] });
      expect(windowOpen).toHaveBeenLastCalledWith(
        expect.stringMatching(new RegExp(`/tasks/${task.id}/interactive$`)),
        task.id,
        '',
      );
    });

    it('opens a tab of the task page', () => {
      utils.openInteractiveTask('task-1', { tab: InteractiveTaskTab.Terminal });
      expect(windowOpen).toHaveBeenCalledWith(
        expect.stringMatching(/\/tasks\/task-1\/interactive\?tab=terminal$/),
        'task-1',
        '',
      );
    });
  });
});
