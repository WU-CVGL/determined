import { V1TaskType } from 'services/api-ts-sdk';
import {
  CommandState,
  CommandTask,
  CommandType,
  ExperimentTask,
  GenericTaskState,
  RunState,
  Task,
} from 'types';

import {
  canBeOpened,
  canConnectToTask,
  canKillGenericTask,
  canOpenShellTerminal,
  canPauseGenericTask,
  canUnpauseGenericTask,
  commandTypeOfTask,
  hasTaskService,
  isExperimentTask,
  shortTaskId,
  taskPhase,
} from './task';

const SampleTask: Task = { id: '', name: '', resourcePool: '', startTime: '' };
const SampleExperimentTask: ExperimentTask = {
  ...SampleTask,
  archived: false,
  parentArchived: false,
  projectId: 0,
  resourcePool: '',
  state: 'ACTIVE' as RunState,
  userId: 345,
  username: '',
  workspaceId: 0,
};
const SampleCommandTask: CommandTask = {
  ...SampleTask,
  resourcePool: '',
  state: 'PENDING' as CommandState,
  type: 'COMMAND' as CommandType,
  userId: 345,
  workspaceId: 0,
};

describe('isExperimentTask', () => {
  it('Experiment Task', () => {
    expect(isExperimentTask(SampleExperimentTask)).toStrictEqual(true);
  });
  it('Command Task', () => {
    expect(isExperimentTask(SampleCommandTask)).toStrictEqual(false);
  });
});

describe('canBeOpened', () => {
  it('Experiment Task', () => {
    expect(canBeOpened(SampleExperimentTask)).toStrictEqual(true);
  });
  it('Terminated Command Task', () => {
    expect(
      canBeOpened({ ...SampleCommandTask, state: 'TERMINATED' as CommandState }),
    ).toStrictEqual(false);
    expect(canBeOpened({ ...SampleCommandTask, state: CommandState.Terminating })).toBe(false);
  });
  it('Command Task that has not ended, with or without a service address', () => {
    expect(canBeOpened(SampleCommandTask)).toStrictEqual(true);
    expect(canBeOpened({ ...SampleCommandTask, serviceAddress: 'test' })).toStrictEqual(true);
    expect(canBeOpened({ ...SampleCommandTask, type: CommandType.Shell })).toBe(true);
  });
});

describe('generic task actions', () => {
  const active = { noPause: false, state: GenericTaskState.Active };

  it('pauses only active tasks that were created pausable', () => {
    expect(canPauseGenericTask(active, true)).toBe(true);
    expect(canPauseGenericTask({ ...active, noPause: true }, true)).toBe(false);
    expect(canPauseGenericTask({ ...active, noPause: undefined }, true)).toBe(false);
    expect(canPauseGenericTask({ ...active, state: GenericTaskState.Paused }, true)).toBe(false);
    expect(canPauseGenericTask(active, false)).toBe(false);
  });

  it('unpauses only paused tasks', () => {
    expect(canUnpauseGenericTask({ state: GenericTaskState.Paused }, true)).toBe(true);
    expect(canUnpauseGenericTask({ state: GenericTaskState.StoppingPaused }, true)).toBe(false);
    expect(canUnpauseGenericTask({ state: GenericTaskState.Active }, true)).toBe(false);
    expect(canUnpauseGenericTask({ state: GenericTaskState.Paused }, false)).toBe(false);
  });

  it('retries a failed unpause whatever the state', () => {
    expect(canUnpauseGenericTask({ state: GenericTaskState.Active }, true, true)).toBe(true);
    expect(canUnpauseGenericTask({ state: GenericTaskState.Active }, false, true)).toBe(false);
  });

  it('kills tasks that have not ended', () => {
    expect(canKillGenericTask({ state: GenericTaskState.Active }, true)).toBe(true);
    expect(canKillGenericTask({ state: GenericTaskState.Paused }, true)).toBe(true);
    expect(canKillGenericTask({ state: GenericTaskState.StoppingPaused }, true)).toBe(true);
    expect(canKillGenericTask({ state: GenericTaskState.Completed }, true)).toBe(false);
    expect(canKillGenericTask({ state: GenericTaskState.Canceled }, true)).toBe(false);
    expect(canKillGenericTask({ state: GenericTaskState.StoppingError }, true)).toBe(false);
    expect(canKillGenericTask({ state: undefined }, true)).toBe(false);
    expect(canKillGenericTask({ state: GenericTaskState.Active }, false)).toBe(false);
  });

  it('shortens task IDs', () => {
    expect(shortTaskId('0b7c5e2a-1f2e-4c3d-9a8b-7c6d5e4f3a2b')).toBe('0b7c5e2a');
  });
});

describe('taskPhase', () => {
  const running = { isReady: false, state: CommandState.Running };

  it('follows the current allocation, the first one listed', () => {
    expect(taskPhase({ allocations: [] })).toBe('waiting');
    expect(taskPhase({ allocations: [running] })).toBe('waiting');
    expect(taskPhase({ allocations: [{ ...running, isReady: true }] })).toBe('ready');
    expect(taskPhase({ allocations: [{ ...running, state: CommandState.Terminated }] })).toBe(
      'ended',
    );
    expect(
      taskPhase({
        allocations: [{ ...running, isReady: true }],
        endTime: '2026-01-01T01:00:00Z',
      }),
    ).toBe('ended');
    // The first allocation is the current one; an ended earlier one does not count.
    expect(
      taskPhase({
        allocations: [
          { ...running, isReady: true },
          { ...running, state: CommandState.Terminated },
        ],
      }),
    ).toBe('ready');
  });
});

describe('commandTypeOfTask', () => {
  it('maps the task types that have a command API', () => {
    expect(commandTypeOfTask(V1TaskType.SHELL)).toBe(CommandType.Shell);
    expect(commandTypeOfTask(V1TaskType.COMMAND)).toBe(CommandType.Command);
    expect(commandTypeOfTask(V1TaskType.TENSORBOARD)).toBe(CommandType.TensorBoard);
    expect(commandTypeOfTask(V1TaskType.NOTEBOOK)).toBe(CommandType.JupyterLab);
    expect(commandTypeOfTask(V1TaskType.GENERIC)).toBeUndefined();
    expect(commandTypeOfTask(V1TaskType.TRIAL)).toBeUndefined();
    expect(commandTypeOfTask(undefined)).toBeUndefined();
  });
});

describe('hasTaskService', () => {
  it('finds a service by name whatever its state', () => {
    expect(hasTaskService([{ name: 'ssh' }, { name: 'jupyter' }], 'jupyter')).toBe(true);
    expect(hasTaskService([{ name: 'ssh' }], 'jupyter')).toBe(false);
    expect(hasTaskService([], 'ssh')).toBe(false);
  });
});

describe('canConnectToTask and canOpenShellTerminal', () => {
  const ssh = { mayConnect: true, name: 'ssh' };
  const jupyter = { mayConnect: true, name: 'jupyter' };

  it.each([canConnectToTask, canOpenShellTerminal])(
    'need an SSH service the master lets the user connect to (%o)',
    (predicate) => {
      expect(predicate([ssh])).toBe(true);
      expect(predicate([jupyter, ssh])).toBe(true);
      expect(predicate([{ ...ssh, mayConnect: false }])).toBe(false);
      expect(predicate([{ ...ssh, mayConnect: false }, jupyter])).toBe(false);
      expect(predicate([jupyter])).toBe(false);
      expect(predicate([])).toBe(false);
    },
  );
});
