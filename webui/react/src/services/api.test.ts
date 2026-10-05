import { DateString } from 'ioTypes';
import {
  Taskv1State,
  V1GenericTask,
  V1GenericTaskState,
  V1GetGenericTasksResponse,
  V1SlotsFilter,
} from 'services/api-ts-sdk';
import { GenericTaskState } from 'types';

import {
  getCommands,
  getExperiments,
  getGenericTask,
  getGenericTaskConfig,
  getGenericTasks,
  getJupyterLabs,
  getShells,
  getTensorBoards,
  killGenericTask,
  pauseGenericTask,
  unpauseGenericTask,
} from './api';
import { detApi } from './apiConfig';

// setupTests mocks services/api for every test; these tests exercise the real wrappers.
vi.unmock('services/api');

const sdkTask = (taskId: string, parentId?: string): V1GenericTask => ({
  description: '',
  jobId: `job-${taskId}`,
  name: taskId,
  noPause: false,
  parentId,
  projectId: 1,
  resourcePool: 'default',
  slots: 1,
  startTime: '2026-01-01T00:00:00Z' as DateString,
  state: V1GenericTaskState.ACTIVE,
  taskId,
  userId: 3,
  username: 'alice',
  workspaceId: 1,
});

const listResponse = (tasks: V1GenericTask[]): V1GetGenericTasksResponse => ({
  pagination: { limit: 0, offset: 0, total: tasks.length },
  tasks,
});

describe('generic task services', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('getGenericTasks passes filters in the API order and decodes the response', async () => {
    const spy = vi
      .spyOn(detApi.Tasks, 'getGenericTasks')
      .mockResolvedValue(listResponse([sdkTask('a')]));

    const response = await getGenericTasks({
      limit: 10,
      offset: 20,
      parentId: 'p',
      states: [GenericTaskState.Active, GenericTaskState.StoppingPaused],
      userIds: [3],
      workspaceId: 4,
    });

    expect(spy).toHaveBeenCalledWith(
      20,
      10,
      undefined,
      [3],
      4,
      [V1GenericTaskState.ACTIVE, V1GenericTaskState.STOPPINGPAUSED],
      'p',
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
    );
    expect(response.tasks[0].state).toBe(GenericTaskState.Active);
    expect(response.pagination.total).toBe(1);
  });

  it('getGenericTasks passes the project, search and GPU filters', async () => {
    const spy = vi.spyOn(detApi.Tasks, 'getGenericTasks').mockResolvedValue(listResponse([]));
    const signal = new AbortController().signal;

    await getGenericTasks(
      { projectId: 5, search: 'sweep', slotsFilter: V1SlotsFilter.CPUONLY },
      { signal },
    );

    expect(spy).toHaveBeenCalledWith(
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      5,
      'sweep',
      V1SlotsFilter.CPUONLY,
      { signal },
    );
  });

  it('killGenericTask sends killFromRoot', async () => {
    const spy = vi.spyOn(detApi.Tasks, 'killGenericTask').mockResolvedValue({});
    await killGenericTask({ taskId: 't' });
    expect(spy).toHaveBeenLastCalledWith('t', { killFromRoot: false, taskId: 't' }, undefined);
    await killGenericTask({ killFromRoot: true, taskId: 't' });
    expect(spy).toHaveBeenLastCalledWith('t', { killFromRoot: true, taskId: 't' }, undefined);
  });

  it('pause and unpause call their endpoints', async () => {
    const pause = vi.spyOn(detApi.Tasks, 'pauseGenericTask').mockResolvedValue({});
    const unpause = vi.spyOn(detApi.Tasks, 'unpauseGenericTask').mockResolvedValue({});
    await pauseGenericTask({ taskId: 't' });
    await unpauseGenericTask({ taskId: 't' });
    expect(pause).toHaveBeenCalledWith('t', undefined);
    expect(unpause).toHaveBeenCalledWith('t', undefined);
  });

  it('keeps the master message of a refused action', async () => {
    vi.spyOn(detApi.Tasks, 'pauseGenericTask').mockRejectedValue(
      new Response(JSON.stringify({ code: 9, message: 'cannot pause task t' }), { status: 400 }),
    );
    await expect(pauseGenericTask({ taskId: 't' })).rejects.toMatchObject({
      publicMessage: 'cannot pause task t',
    });
  });

  it('getGenericTaskConfig parses the config', async () => {
    vi.spyOn(detApi.Tasks, 'getGenericTaskConfig').mockResolvedValue({
      config: '{"name":"eval"}',
    });
    expect(await getGenericTaskConfig({ taskId: 't' })).toStrictEqual({ name: 'eval' });
  });

  describe('getGenericTask', () => {
    it('filters the list by the task ID', async () => {
      const spy = vi
        .spyOn(detApi.Tasks, 'getGenericTasks')
        .mockResolvedValue(listResponse([sdkTask('b', 'p')]));
      const task = await getGenericTask('b');
      expect(task?.taskId).toBe('b');
      expect(task?.parentId).toBe('p');
      expect(spy).toHaveBeenCalledTimes(1);
      expect(spy.mock.calls[0][7]).toStrictEqual(['b']);
      expect(spy.mock.calls[0][3]).toBeUndefined();
      expect(spy.mock.calls[0][6]).toBeUndefined();
    });

    it('returns undefined for a task it cannot see', async () => {
      vi.spyOn(detApi.Tasks, 'getGenericTasks').mockResolvedValue(listResponse([]));
      expect(await getGenericTask('x')).toBeUndefined();
    });
  });
});

describe('dashboard list services', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('getExperiments passes the workspace and GPU filters', async () => {
    const spy = vi
      .spyOn(detApi.Experiments, 'getExperiments')
      .mockResolvedValue({ experiments: [], pagination: { total: 0 } });

    await getExperiments({ slotsFilter: V1SlotsFilter.GPU, workspaceId: 7 });

    const args = spy.mock.calls[0];
    // workspaceId and slotsFilter come right before the request options.
    expect(args.slice(-3, -1)).toStrictEqual([7, V1SlotsFilter.GPU]);
  });

  it('getExperiments filters by user IDs, as given or as user strings', async () => {
    const spy = vi
      .spyOn(detApi.Experiments, 'getExperiments')
      .mockResolvedValue({ experiments: [], pagination: { total: 0 } });

    await getExperiments({ userIds: [3] });
    await getExperiments({ users: ['4'] });
    await getExperiments({});

    // userIds is the SDK's 11th argument.
    expect(spy.mock.calls.map((args) => args[10])).toStrictEqual([[3], [4], undefined]);
  });

  it('getExperiments sends neither filter when they are unset', async () => {
    const spy = vi
      .spyOn(detApi.Experiments, 'getExperiments')
      .mockResolvedValue({ experiments: [], pagination: { total: 0 } });

    await getExperiments({});

    expect(spy.mock.calls[0].slice(-3, -1)).toStrictEqual([undefined, undefined]);
  });

  const SDK_TASK = {
    description: 'task',
    id: 't1',
    jobId: 'j1',
    resourcePool: 'default',
    slots: 2,
    startTime: '2026-01-01T00:00:00Z' as DateString,
    state: Taskv1State.RUNNING,
    username: 'alice',
    workspaceId: 1,
  };

  /* A task list wrapper passes the request's abort signal on and decodes the slots. */
  const expectSignalAndSlots = async (
    list: typeof getCommands,
    spy: { mock: { calls: unknown[][] } },
  ) => {
    const signal = new AbortController().signal;
    const tasks = await list({ workspaceId: 1 }, { signal });
    expect(spy.mock.calls[0].at(-1)).toStrictEqual({ signal });
    expect(tasks[0].slots).toBe(2);
  };

  it('getCommands forwards the abort signal and reads the slots', async () => {
    const spy = vi.spyOn(detApi.Commands, 'getCommands').mockResolvedValue({
      commands: [SDK_TASK],
    });
    await expectSignalAndSlots(getCommands, spy);
  });

  it('getJupyterLabs forwards the abort signal and reads the slots', async () => {
    const spy = vi.spyOn(detApi.Notebooks, 'getNotebooks').mockResolvedValue({
      notebooks: [SDK_TASK],
    });
    await expectSignalAndSlots(getJupyterLabs, spy);
  });

  it('getShells forwards the abort signal and reads the slots', async () => {
    const spy = vi.spyOn(detApi.Shells, 'getShells').mockResolvedValue({ shells: [SDK_TASK] });
    await expectSignalAndSlots(getShells, spy);
  });

  it('getTensorBoards forwards the abort signal and reads the slots', async () => {
    const spy = vi.spyOn(detApi.TensorBoards, 'getTensorboards').mockResolvedValue({
      tensorboards: [SDK_TASK],
    });
    await expectSignalAndSlots(getTensorBoards, spy);
  });
});
