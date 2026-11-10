import {
  killableCommandStates,
  killableGenericTaskStates,
  killableRunStates,
  terminalCommandStates,
} from 'constants/states';
import { V1TaskService, V1TaskServiceState, V1TaskType } from 'services/api-ts-sdk';
import * as Type from 'types';
import { CommandState, RunState, State } from 'types';

import { runStateSortValues } from './experiment';

/**
 * Whether to offer the named service of a task: the task has it and the master lets the user
 * connect to it (the task's owner, or an admin), which is also who may read its credential. The
 * master enforces the same rule.
 */
export const canOpenTaskService = (
  services: Pick<V1TaskService, 'mayConnect' | 'name'>[],
  name: string,
): boolean => {
  return services.some((s) => s.name === name && s.mayConnect);
};

/** Whether to offer a browser terminal in a task: its SSH service, by the rule of every service. */
export const canOpenShellTerminal = (
  services: Pick<V1TaskService, 'mayConnect' | 'name'>[],
): boolean => {
  return canOpenTaskService(services, 'ssh');
};

/**
 * Whether to offer Connect via CLI (`det shell open`) for a task: the same rule as the terminal,
 * since the master gives the SSH service's key to the same users.
 */
export const canConnectToTask = (
  services: Pick<V1TaskService, 'mayConnect' | 'name'>[],
): boolean => {
  return canOpenShellTerminal(services);
};

/** Where a task is for the panes that connect to it: waiting for it, ready, or ended. */
export type TaskPhase = 'waiting' | 'ready' | 'ended';

/* The phase of a task from its current allocation, the first one the master lists. */
export const taskPhase = (task: Pick<Type.TaskItem, 'allocations' | 'endTime'>): TaskPhase => {
  const allocation = task.allocations[0];
  if (task.endTime || allocation?.state === CommandState.Terminated) return 'ended';
  return allocation?.isReady ? 'ready' : 'waiting';
};

/** The command type of a task's type, for the task APIs that take one; none for other tasks. */
export const commandTypeOfTask = (taskType?: V1TaskType): Type.CommandType | undefined => {
  switch (taskType) {
    case V1TaskType.COMMAND:
      return Type.CommandType.Command;
    case V1TaskType.SHELL:
      return Type.CommandType.Shell;
    default:
      return undefined;
  }
};

/**
 * Whether a service's state has settled: it is ready, or it will not be (it has exited, or it
 * cannot run). A service that is starting, or that the container has not reported yet, has not.
 */
export const taskServiceSettled = (state: V1TaskServiceState): boolean => {
  return state !== V1TaskServiceState.STARTING && state !== V1TaskServiceState.UNSPECIFIED;
};

/** Whether a task has a page to open: an experiment's, or the task page of a task that has not ended. */
export const canBeOpened = (task: Type.AnyTask): boolean => {
  if (isExperimentTask(task)) return true;
  return !terminalCommandStates.has(task.state);
};

/* eslint-disable-next-line @typescript-eslint/no-explicit-any */
export function getRandomElementOfEnum(e: any): any {
  const keys = Object.keys(e);
  return e[keys.random()];
}

export const sampleUsers = [
  { displayName: '', id: 0, username: 'admin' },
  { displayName: '', id: 1, username: 'determined' },
  { displayName: '', id: 2, username: 'hamid' },
];

function generateTask(idx: number): Type.Task & Type.RecentEvent {
  const now = Date.now();
  const range = Math.random() * 2 * 356 * 24 * 60 * 60 * 1000;
  const startTime = new Date(now - range).toString();
  return {
    id: `${idx}`,
    lastEvent: {
      date: startTime,
      name: 'opened',
    },
    name: `${idx}`,
    resourcePool: `ResourcePool-${Math.floor(Math.random() * 3)}`,
    startTime,
    url: '#',
  };
}

export function generateExperimentTask(idx: number): Type.RecentExperimentTask {
  const state = getRandomElementOfEnum(Type.RunState);
  const task = generateTask(idx);
  const progress = Math.random();
  const user = sampleUsers.random();
  return {
    ...task,
    archived: false,
    parentArchived: false,
    progress,
    projectId: 1,
    state: state as Type.RunState,
    url: '#',
    userId: user.id,
    username: user.username,
    workspaceId: 1,
  };
}

export const generateExperiment = (id = 1): Type.FullExperimentItem => {
  const experimentTask = generateExperimentTask(id);
  const user = sampleUsers.random();
  const config = {
    name: experimentTask.name,
    resources: {},
    searcher: { metric: 'val_error', name: 'single', smallerIsBetter: true },
  };
  return {
    ...experimentTask,
    config: {
      checkpointPolicy: 'best',
      checkpointStorage: {
        hostPath: '/tmp',
        saveExperimentBest: 0,
        saveTrialBest: 1,
        saveTrialLatest: 1,
        storagePath: 'determined-integration-checkpoints',
        type: 'shared_fs',
      },
      hyperparameters: {},
      maxRestarts: 5,
      name: experimentTask.name ?? '',
      resources: {},
      searcher: { metric: 'val_error', name: 'single', smallerIsBetter: true },
    },
    configRaw: config,
    hyperparameters: {},
    id: id,
    jobId: id.toString(),
    labels: [],
    name: experimentTask.name ?? '',
    numTrials: Math.round(Math.random() * 60000),
    projectId: 1,
    resourcePool: `ResourcePool-${Math.floor(Math.random() * 3)}`,
    searcherType: 'single',
    userId: user.id,
  };
};

export const generateExperiments = (count = 30): Type.FullExperimentItem[] => {
  return new Array(Math.floor(count)).fill(null).map((_, idx) => generateExperiment(idx));
};

// Differentiate Task from Experiment.
export const isCommandTask = (obj: Type.Command | Type.CommandTask): obj is Type.CommandTask => {
  return 'type' in obj;
};

export const isExperimentTask = (task: Type.AnyTask): task is Type.ExperimentTask => {
  return 'archived' in task && !('type' in task);
};

/* The first group of a task's UUID, as the tables show it. */
export const shortTaskId = (taskId: string): string => taskId.split('-')[0];

/*
 * A generic task can be paused only while active and only if it was created pausable, because
 * unpausing runs its entrypoint again from the start. The master refuses anything else.
 */
export const canPauseGenericTask = (
  task: { noPause?: boolean; state?: Type.GenericTaskState },
  canControl: boolean,
): boolean => {
  return canControl && task.noPause === false && task.state === Type.GenericTaskState.Active;
};

/*
 * A paused task can be unpaused. After an unpause failed part way, the master finishes it when
 * unpause is retried on the same root task, also once that task is active again, so a retry is
 * offered whatever the state and the master decides.
 */
export const canUnpauseGenericTask = (
  task: { state?: Type.GenericTaskState },
  canControl: boolean,
  isRetry = false,
): boolean => {
  return canControl && (isRetry || task.state === Type.GenericTaskState.Paused);
};

export const canKillGenericTask = (
  task: { state?: Type.GenericTaskState },
  canControl: boolean,
): boolean => {
  return canControl && !!task.state && killableGenericTaskStates.has(task.state);
};

export const isTaskKillable = (task: { state: State }, canModifyWorkspaceNSC: boolean): boolean => {
  return (
    canModifyWorkspaceNSC &&
    (killableRunStates.includes(task.state as Type.RunState) ||
      killableCommandStates.includes(task.state as Type.CommandState))
  );
};

const commandStateSortOrder: CommandState[] = [
  CommandState.Pulling,
  CommandState.Starting,
  CommandState.Running,
  CommandState.Terminating,
  CommandState.Terminated,
];

const commandStateSortValues: Map<CommandState, number> = new Map(
  commandStateSortOrder.map((state, idx) => [state, idx]),
);

export const taskStateSorter = (a: State, b: State): number => {
  // FIXME this is O(n) we can do it in constant time.
  // What is the right typescript way of doing it?
  const aValue = Object.values(RunState).includes(a as RunState)
    ? runStateSortValues.get(a as RunState) || 0
    : commandStateSortValues.get(a as CommandState) || 0;
  const bValue = Object.values(RunState).includes(b as RunState)
    ? runStateSortValues.get(b as RunState) || 0
    : commandStateSortValues.get(b as CommandState) || 0;
  return aValue - bValue;
};
