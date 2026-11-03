import {
  mapV1Command,
  mapV1ExperimentList,
  mapV1GenericTask,
  mapV1Shell,
  mapV1TensorBoard,
} from 'services/decoder';

import * as Sdk from './api-ts-sdk';

const submitted = '2026-11-02T05:25:48.830Z';
const assigned = '2026-11-02T07:04:22.523Z';

it('keeps submitted and assigned times distinct for every command kind, including an older master', () => {
  const wire = {
    allocationStartTime: assigned,
    description: 'task',
    id: 'task',
    startTime: submitted,
    state: Sdk.Taskv1State.RUNNING,
  };
  const decode = (item: typeof wire) => [
    mapV1Command(item as Sdk.V1Command),
    mapV1Shell(item as Sdk.V1Shell),
    mapV1TensorBoard(item as Sdk.V1Tensorboard),
  ];
  for (const task of decode(wire)) {
    expect(task.startTime).toBe(submitted);
    expect(task.allocationStartTime).toBe(assigned);
  }
  for (const task of decode({ ...wire, allocationStartTime: undefined as unknown as string })) {
    expect(task.startTime).toBe(submitted);
    expect(task.allocationStartTime).toBeUndefined();
  }
});

it('preserves the independent timestamps for generic tasks and experiment list rows', () => {
  const wire = {
    allocationStartTime: assigned,
    startTime: submitted,
    state: Sdk.V1GenericTaskState.ACTIVE,
    taskId: 'generic',
  } as Sdk.V1GenericTask;
  expect(mapV1GenericTask(wire)).toMatchObject({
    allocationStartTime: assigned,
    startTime: submitted,
  });
  const experiment = {
    allocationStartTime: assigned,
    config: {},
    id: 1,
    startTime: submitted,
  } as Sdk.V1Experiment;
  expect(mapV1ExperimentList([experiment])[0]).toMatchObject({
    allocationStartTime: assigned,
    startTime: submitted,
  });
});
