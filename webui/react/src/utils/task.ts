import _ from 'lodash';

import {
  killableCommandStates,
  killableGenericTaskStates,
  killableRunStates,
  terminalCommandStates,
} from 'constants/states';
import { LaunchTensorBoardParams } from 'services/types';
import * as Type from 'types';
import { CommandState, RunState, State } from 'types';

import { runStateSortValues } from './experiment';

/**
 * Whether to offer a browser terminal for a task: a running shell of the user's own, or any
 * running shell for an admin. The master enforces the same rule.
 */
export const canOpenShellTerminal = (
  task: Pick<Type.CommandTask, 'state' | 'type' | 'userId'>,
  user?: Pick<Type.DetailedUser, 'id' | 'isAdmin'>,
): boolean => {
  if (!user || task.type !== Type.CommandType.Shell || task.state !== CommandState.Running) {
    return false;
  }
  return user.isAdmin || user.id === task.userId;
};

/**
 * Whether to offer Connect (a JupyterLab's address with its token) or Connect via CLI (a shell's
 * `det shell open`) for a task: a running JupyterLab or shell of the user's own, or of anyone for
 * an admin. The master gives a notebook's token and a shell's key only to them.
 */
export const canConnectToTask = (
  task: Pick<Type.CommandTask, 'state' | 'type' | 'userId'>,
  user?: Pick<Type.DetailedUser, 'id' | 'isAdmin'>,
): boolean => {
  const connectable: Type.CommandType[] = [Type.CommandType.JupyterLab, Type.CommandType.Shell];
  if (!user || !connectable.includes(task.type) || task.state !== CommandState.Running) {
    return false;
  }
  return user.isAdmin || user.id === task.userId;
};

export const canBeOpened = (task: Type.AnyTask): boolean => {
  if (isExperimentTask(task)) return true;
  if (terminalCommandStates.has(task.state)) return false;
  return !!task.serviceAddress;
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
      name: experimentTask.name,
      resources: {},
      searcher: { metric: 'val_error', name: 'single', smallerIsBetter: true },
    },
    configRaw: config,
    hyperparameters: {},
    id: id,
    jobId: id.toString(),
    labels: [],
    name: experimentTask.name,
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

export const isTaskKillable = (
  task: Type.AnyTask | Type.FullExperimentItem,
  canModifyWorkspaceNSC: boolean,
): boolean => {
  return (
    canModifyWorkspaceNSC &&
    (killableRunStates.includes(task.state as Type.RunState) ||
      killableCommandStates.includes(task.state as Type.CommandState))
  );
};

// Checks whether tensorboard source matches a given source list.
export const tensorBoardMatchesSource = (
  tensorBoard: Type.CommandTask,
  source: LaunchTensorBoardParams,
): boolean => {
  if (source.experimentIds) {
    source.experimentIds?.sort();
    tensorBoard.misc?.experimentIds?.sort();

    if (_.isEqual(tensorBoard.misc?.experimentIds, source.experimentIds)) {
      return true;
    }
  }

  if (source.trialIds) {
    source.trialIds?.sort();
    tensorBoard.misc?.trialIds?.sort();

    if (_.isEqual(tensorBoard.misc?.trialIds, source.trialIds)) {
      return true;
    }
  }

  return false;
};

const commandStateSortOrder: CommandState[] = [
  CommandState.Pulling,
  CommandState.Starting,
  CommandState.Running,
  CommandState.Waiting,
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
