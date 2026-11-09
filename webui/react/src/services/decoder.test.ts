import { gpuTopologyCase } from 'fixtures/gpuTopologyCases';
import hparams from 'fixtures/hyperparameter-configs.json';
import experimentResps from 'fixtures/responses/experiment-details/set-a.json';
import * as ioTypes from 'ioTypes';
import { DateString } from 'ioTypes';
import {
  Devicev1Type,
  Experimentv1State,
  Taskv1State,
  V1Agent,
  V1Experiment,
  V1ExperimentActionResult,
  V1FlatRun,
  V1GenericTask,
  V1GenericTaskState,
  V1JobCapabilities,
  V1RunActionResult,
  V1Task,
  V1TaskType,
} from 'services/api-ts-sdk';
import * as decoder from 'services/decoder';
import { CommandState, GenericTaskState } from 'types';

type FailReport<T = unknown> = { error: Error; sample: T };

const tryOnSamples = <T = unknown>(samples: T[], fn: (sample: T) => void): FailReport[] => {
  const fails: FailReport[] = [];
  samples.forEach((sample) => {
    try {
      fn(sample);
    } catch (e) {
      fails.push({ error: e as Error, sample });
    }
  });
  if (fails.length > 0) {
    const { sample, error } = fails.last();
    /* eslint-disable no-console */
    console.error(error);
    console.log('Sample:', sample);
    /* eslint-enable no-console */
  }
  return fails;
};

describe('Decoder', () => {
  it('Should decode seeded hyperparameters', () => {
    const fails = tryOnSamples(hparams, (hparam) => {
      ioTypes.decode<ioTypes.ioTypeHyperparameters>(ioTypes.ioHyperparameters, hparam);
    });
    expect(fails).toHaveLength(0);
  });

  it('Should decode experiment configs', () => {
    const fails = tryOnSamples(
      experimentResps.map((r) => r.config),
      (config) => {
        ioTypes.decode<ioTypes.ioTypeExperimentConfig>(ioTypes.ioExperimentConfig, config);
      },
    );
    expect(fails).toHaveLength(0);
  });

  describe('mapV1ActionResults', () => {
    it('should work with Sdk.V1ExperimentActionResult[] input', () => {
      const result: V1ExperimentActionResult[] = [
        { error: '', id: 1 },
        { error: '', id: 2 },
        { error: 'error', id: 3 },
      ];

      const expected = decoder.mapV1ActionResults(result);
      expect(expected).toStrictEqual({
        failed: [{ error: 'error', id: 3 }],
        successful: [1, 2],
      });
    });

    it('should work with Sdk.V1RunActionResult[] input', () => {
      const result: V1RunActionResult[] = [
        { error: '', id: 1 },
        { error: '', id: 2 },
        { error: 'error', id: 3 },
      ];

      const expected = decoder.mapV1ActionResults(result);
      expect(expected).toStrictEqual({
        failed: [{ error: 'error', id: 3 }],
        successful: [1, 2],
      });
    });

    it('should work with empty input', () => {
      const expected = decoder.mapV1ActionResults([]);
      expect(expected).toStrictEqual({
        failed: [],
        successful: [],
      });
    });

    it('should work with all successful input', () => {
      const result: V1RunActionResult[] = [
        { error: '', id: 1 },
        { error: '', id: 2 },
        { error: '', id: 3 },
        { error: '', id: 4 },
        { error: '', id: 5 },
      ];

      const expected = decoder.mapV1ActionResults(result);
      expect(expected).toStrictEqual({
        failed: [],
        successful: [1, 2, 3, 4, 5],
      });
    });

    it('should work with all failed input', () => {
      const result: V1RunActionResult[] = [
        { error: 'oh no', id: 1 },
        { error: 'yare yare', id: 2 },
        { error: 'error', id: 3 },
        { error: 'a', id: 4 },
        { error: 'エラー', id: 5 },
      ];

      const expected = decoder.mapV1ActionResults(result);
      expect(expected).toStrictEqual({
        failed: [
          { error: 'oh no', id: 1 },
          { error: 'yare yare', id: 2 },
          { error: 'error', id: 3 },
          { error: 'a', id: 4 },
          { error: 'エラー', id: 5 },
        ],
        successful: [],
      });
    });
  });

  describe('shells, commands and TensorBoards', () => {
    const wire = (endTime?: string) => ({
      description: 'train',
      endTime: endTime as DateString | undefined,
      id: 'task-1',
      jobId: 'job-1',
      resourcePool: 'default',
      startTime: '2026-01-01T00:00:00Z' as DateString,
      state: endTime ? Taskv1State.TERMINATED : Taskv1State.RUNNING,
      username: 'alice',
      workspaceId: 1,
    });
    const mappers = [decoder.mapV1Command, decoder.mapV1Shell, decoder.mapV1TensorBoard];

    it('should carry the end time once ended, none while running', () => {
      mappers.forEach((map) => {
        expect(map(wire()).endTime).toBeUndefined();
        expect(map(wire('2026-01-01T01:00:00Z')).endTime).toBe('2026-01-01T01:00:00Z');
      });
    });

    it('should carry the job, whose sharing the Sharing action opens', () => {
      mappers.forEach((map) => expect(map(wire()).jobId).toBe('job-1'));
    });

    it('should carry the sources of a shell that has them', () => {
      expect(decoder.mapV1Shell({ ...wire(), experimentIds: [4], trialIds: [] }).misc).toEqual({
        experimentIds: [4],
        trialIds: [],
      });
      expect(decoder.mapV1Shell({ ...wire(), trialIds: [9] }).misc).toEqual({
        experimentIds: [],
        trialIds: [9],
      });
      expect(decoder.mapV1Shell(wire())).not.toHaveProperty('misc');
      expect(decoder.mapV1Shell({ ...wire(), experimentIds: [], trialIds: [] })).not.toHaveProperty(
        'misc',
      );
    });
  });

  describe('generic tasks', () => {
    const sdkTask: V1GenericTask = {
      allocationId: 'alloc-1',
      description: 'evaluates the checkpoints of run 12',
      displayName: 'Alice Chen',
      forkedFrom: '',
      jobId: 'job-1',
      name: 'eval-sweep',
      noPause: false,
      parentId: 'parent-1',
      projectId: 1,
      resourcePool: 'default',
      slots: 2,
      startTime: '2026-01-01T00:00:00Z' as DateString,
      state: V1GenericTaskState.STOPPINGPAUSED,
      taskId: 'task-1',
      userId: 3,
      username: 'alice',
      workspaceId: 4,
    };

    it('should strip the state prefix', () => {
      expect(decoder.mapV1GenericTaskState(V1GenericTaskState.ACTIVE)).toBe(
        GenericTaskState.Active,
      );
      expect(decoder.mapV1GenericTaskState(V1GenericTaskState.STOPPINGPAUSED)).toBe(
        GenericTaskState.StoppingPaused,
      );
      expect(decoder.mapV1GenericTaskState('BOGUS' as V1GenericTaskState)).toBe(
        GenericTaskState.Unspecified,
      );
    });

    it('should encode states for requests', () => {
      Object.values(GenericTaskState).forEach((state) => {
        const encoded = decoder.encodeGenericTaskState(state);
        expect(Object.values(V1GenericTaskState)).toContain(encoded);
        expect(decoder.mapV1GenericTaskState(encoded)).toBe(state);
      });
    });

    it('should map a listed generic task', () => {
      expect(decoder.mapV1GenericTask(sdkTask)).toStrictEqual({
        allocationId: 'alloc-1',
        allocationStartTime: undefined,
        capabilities: undefined,
        description: 'evaluates the checkpoints of run 12',
        displayName: 'Alice Chen',
        endTime: undefined,
        forkedFrom: undefined,
        jobId: 'job-1',
        name: 'eval-sweep',
        noPause: false,
        parentId: 'parent-1',
        projectId: 1,
        resourcePool: 'default',
        slots: 2,
        startTime: '2026-01-01T00:00:00Z',
        state: GenericTaskState.StoppingPaused,
        taskId: 'task-1',
        userId: 3,
        username: 'alice',
        workspaceId: 4,
      });
    });

    it('should name an unnamed task and paginate', () => {
      const response = decoder.mapV1GenericTasksResponse({
        pagination: { endIndex: 1, limit: 10, offset: 0, startIndex: 0, total: 11 },
        tasks: [{ ...sdkTask, name: '' }],
      });
      expect(response.pagination).toStrictEqual({ limit: 10, offset: 0, total: 11 });
      expect(response.tasks[0].name).toBe('Generic Task task-1');
    });

    it('should carry the generic fields and allocations of GetTask', () => {
      const task: V1Task = {
        allocations: [
          {
            allocationId: 'task-1.1',
            endTime: '2026-01-01T01:00:00Z',
            exitReason: 'allocation stopped after resources exited successfully',
            slots: 2,
            startTime: '2026-01-01T00:00:00Z',
            state: 'STATE_TERMINATED',
            statusCode: 0,
            taskId: 'task-1',
          },
        ],
        forkedFrom: 'origin-1',
        noPause: true,
        parentId: '',
        startTime: '2026-01-01T00:00:00Z' as DateString,
        taskId: 'task-1',
        taskState: V1GenericTaskState.COMPLETED,
        taskType: V1TaskType.GENERIC,
      };
      const item = decoder.mapV1Task(task);
      expect(item.taskState).toBe(GenericTaskState.Completed);
      expect(item.parentId).toBeUndefined();
      expect(item.forkedFrom).toBe('origin-1');
      expect(item.noPause).toBe(true);
      expect(item.taskType).toBe(V1TaskType.GENERIC);
      expect(item.allocations[0]).toMatchObject({
        allocationId: 'task-1.1',
        exitReason: 'allocation stopped after resources exited successfully',
        slots: 2,
        state: CommandState.Terminated,
        statusCode: 0,
      });
    });

    it('should parse the config JSON string', () => {
      expect(decoder.mapGenericTaskConfig('{"entrypoint":["python","eval.py"]}')).toStrictEqual({
        entrypoint: ['python', 'eval.py'],
      });
      expect(() => decoder.mapGenericTaskConfig('[1]')).toThrow();
    });
  });
  describe('jsonToAgents', () => {
    it('should keep the GPU topology and the slot draining flag', () => {
      const gpuTopology = gpuTopologyCase('node01 with the exclude list');
      const agent: V1Agent = {
        enabled: false,
        gpuTopology,
        id: 'node01',
        registeredTime: '2026-01-01T00:00:00Z' as DateString,
        resourcePools: ['pool'],
        slots: {
          '/agents/node01/slots/0': {
            device: { brand: 'GPU', id: 0, type: Devicev1Type.CUDA, uuid: 'GPU-a' },
            draining: true,
            enabled: false,
            id: '0',
          },
          '/agents/node01/slots/1': {
            device: { brand: 'GPU', id: 1, type: Devicev1Type.CUDA, uuid: 'GPU-b' },
            enabled: true,
            id: '1',
          },
        },
        slotStats: { brandStats: {}, typeStats: {} },
      };
      const [decoded] = decoder.jsonToAgents([agent]);
      expect(decoded.gpuTopology).toStrictEqual(gpuTopology);
      expect(decoded.resources.map((r) => [r.id, r.enabled, r.draining])).toEqual([
        ['0', false, true],
        ['1', true, undefined],
      ]);
      expect(decoder.jsonToAgents([{ ...agent, gpuTopology: undefined }])[0].gpuTopology).toBe(
        undefined,
      );
    });
  });
  describe('job privacy', () => {
    const hidden: V1JobCapabilities = {
      manageSharing: false,
      viewConfig: false,
      viewLogs: false,
      viewTitle: false,
    };
    const configHidden: V1JobCapabilities = { ...hidden, viewLogs: true, viewTitle: true };
    const sdkTaskOf = (capabilities: V1JobCapabilities): V1GenericTask => ({
      capabilities,
      description: 'evaluates',
      jobId: 'job-1',
      name: 'eval-sweep',
      noPause: true,
      projectId: 1,
      resourcePool: 'default',
      slots: 1,
      startTime: '2026-01-01T00:00:00Z' as DateString,
      state: V1GenericTaskState.ACTIVE,
      taskId: 'task-1',
      userId: 3,
      username: 'alice',
      workspaceId: 4,
    });

    // An experiment as the master sends it to a viewer it hides the title and config from.
    const hiddenExperiment: V1Experiment = {
      archived: false,
      capabilities: hidden,
      config: null,
      description: '',
      id: 12,
      jobId: 'job-12',
      labels: [],
      name: '',
      notes: '',
      numTrials: 1,
      originalConfig: '',
      projectId: 1,
      projectOwnerId: 1,
      searcherMetric: 'loss',
      searcherType: 'single',
      startTime: '2026-01-01T00:00:00Z' as DateString,
      state: Experimentv1State.ACTIVE,
      username: 'alice',
    };

    it('should omit a hidden title and keep the public fields of an experiment', () => {
      const item = decoder.mapV1Experiment(hiddenExperiment);
      expect(item.name).toBeUndefined();
      expect(item.labels).toEqual([]);
      expect(item.config).toBeUndefined();
      expect(item.capabilities).toEqual(hidden);
      expect(item.id).toBe(12);
      expect(item.username).toBe('alice');
      expect(item.searcherType).toBe('single');
    });

    it('should tolerate an experiment without name and labels', () => {
      const { labels: _labels, name: _name, ...rest } = hiddenExperiment;
      const item = decoder.mapV1Experiment(rest as V1Experiment);
      expect(item.name).toBeUndefined();
      expect(item.labels).toEqual([]);
    });

    it('should not decode the config of an experiment detail that omits it', () => {
      const detail = decoder.mapV1GetExperimentDetailsResponse({
        config: null,
        experiment: { ...hiddenExperiment, capabilities: configHidden, name: 'mnist' },
        jobSummary: undefined,
      } as unknown as Parameters<typeof decoder.mapV1GetExperimentDetailsResponse>[0]);
      expect(detail.config).toBeUndefined();
      expect(detail.configRaw).toBeUndefined();
      expect(detail.originalConfig).toBeUndefined();
      expect(detail.name).toBe('mnist');
    });

    it('should not decode a config the capabilities deny', () => {
      const full = decoder.mapV1Experiment(
        { ...hiddenExperiment, capabilities: configHidden },
        undefined,
        { resources: { slots_per_trial: 2 } },
      );
      expect(full.config).toBeUndefined();
      expect(full.configRaw).toBeUndefined();
    });

    it('should decode the config of an experiment shared in full', () => {
      const config = experimentResps[0].config;
      const full = decoder.mapV1Experiment(
        { ...hiddenExperiment, capabilities: { ...hidden, viewConfig: true, viewTitle: true } },
        undefined,
        config,
      );
      expect(full.configRaw).toBe(config);
      expect(full.config?.searcher.name).toBe(config.searcher.name);
    });

    it('should omit the hidden title of a shell, command and generic task', () => {
      const wire = {
        capabilities: hidden,
        description: '',
        id: 'task-1',
        jobId: 'job-1',
        resourcePool: 'default',
        startTime: '2026-01-01T00:00:00Z' as DateString,
        state: Taskv1State.RUNNING,
        username: 'alice',
        workspaceId: 1,
      };
      [decoder.mapV1Command, decoder.mapV1Shell].forEach((map) => {
        const task = map(wire);
        expect(task.name).toBeUndefined();
        expect(task.capabilities).toEqual(hidden);
      });
      const generic = decoder.mapV1GenericTask({ ...sdkTaskOf(hidden), description: '', name: '' });
      expect(generic.name).toBeUndefined();
      expect(generic.capabilities).toEqual(hidden);
    });

    it('should keep the title of a shared task', () => {
      const shared = { ...hidden, viewTitle: true };
      const generic = decoder.mapV1GenericTask({ ...sdkTaskOf(shared), name: '' });
      expect(generic.name).toBe('Generic Task task-1');
    });

    it('should omit the hidden experiment name of a run', () => {
      const run = decoder.decodeV1FlatRun({
        archived: false,
        checkpointCount: 0,
        experiment: {
          capabilities: hidden,
          description: '',
          id: 12,
          isMultitrial: false,
          name: '',
          progress: 0,
          resourcePool: 'default',
          searcherMetric: 'loss',
          searcherType: 'single',
          unmanaged: false,
        },
        id: 3,
        parentArchived: false,
        projectId: 1,
        projectName: 'Uncategorized',
        startTime: '2026-01-01T00:00:00Z' as DateString,
        state: Experimentv1State.ACTIVE,
        summaryMetrics: {},
        workspaceId: 1,
        workspaceName: 'Uncategorized',
      } as V1FlatRun);
      expect(run.experiment?.name).toBeUndefined();
      expect(run.experiment?.capabilities).toEqual(hidden);
    });
  });
});
