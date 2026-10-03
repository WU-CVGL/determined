import { CommandState, CommandTask, CommandType, ExperimentTask, RunState, Task } from 'types';

import { canBeOpened, canOpenShellTerminal, isExperimentTask } from './task';

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
  });
  it('Command Task without service address', () => {
    expect(canBeOpened(SampleCommandTask)).toStrictEqual(false);
  });
  it('Command Task with service address', () => {
    expect(canBeOpened({ ...SampleCommandTask, serviceAddress: 'test' })).toStrictEqual(true);
  });
});

describe('canOpenShellTerminal', () => {
  const shell = { state: CommandState.Running, type: CommandType.Shell, userId: 5 };
  const owner = { id: 5, isAdmin: false };

  it('allows the owner and admins to open running shells', () => {
    expect(canOpenShellTerminal(shell, owner)).toBe(true);
    expect(canOpenShellTerminal(shell, { id: 1, isAdmin: true })).toBe(true);
    expect(canOpenShellTerminal(shell, { id: 6, isAdmin: false })).toBe(false);
    expect(canOpenShellTerminal(shell, undefined)).toBe(false);
  });

  it('only offers running shells', () => {
    expect(canOpenShellTerminal({ ...shell, state: CommandState.Queued }, owner)).toBe(false);
    expect(canOpenShellTerminal({ ...shell, state: CommandState.Terminated }, owner)).toBe(false);
    expect(canOpenShellTerminal({ ...shell, type: CommandType.JupyterLab }, owner)).toBe(false);
  });
});
