import * as Api from 'services/api-ts-sdk';
import { CommandType, JobType, ResourcePool, ResourceType } from 'types';

import * as utils from './job';

describe('Job Utilities', () => {
  describe('jobTypeIconName', () => {
    it('should support experiment and command types', () => {
      expect(utils.jobTypeIconName(JobType.EXPERIMENT)).toEqual('experiment');
      expect(utils.jobTypeIconName(JobType.NOTEBOOK)).toEqual(CommandType.JupyterLab);
    });
    it('should not show generic tasks as experiments', () => {
      expect(utils.jobTypeIconName(JobType.GENERIC)).toEqual('tasks');
    });
  });

  describe('jobTypeLabel', () => {
    it('should label generic tasks', () => {
      expect(utils.jobTypeLabel(JobType.GENERIC)).toEqual('Generic Task');
      expect(utils.jobTypeLabel(JobType.EXPERIMENT)).toEqual('Experiment');
    });
    it('should spell JupyterLab and TensorBoard as the WebUI does', () => {
      expect(utils.jobTypeLabel(JobType.NOTEBOOK)).toEqual('JupyterLab');
      expect(utils.jobTypeLabel(JobType.TENSORBOARD)).toEqual('TensorBoard');
      expect(utils.jobTypeLabel(JobType.SHELL)).toEqual('Shell');
      expect(utils.jobTypeLabel(JobType.COMMAND)).toEqual('Command');
    });
  });

  describe('genericJobLabel', () => {
    const taskId = '0b7c5e2a-1f2e-4c3d-9a8b-7c6d5e4f3a2b';
    it('should add the short task ID to a named task', () => {
      expect(utils.genericJobLabel('eval-sweep', taskId)).toEqual('eval-sweep (0b7c5e2a)');
    });
    it('should not repeat the ID of the default name', () => {
      expect(utils.genericJobLabel(`Generic Task ${taskId}`, taskId)).toEqual(
        `Generic Task ${taskId}`,
      );
      expect(utils.genericJobLabel('', taskId)).toEqual(`Generic Task ${taskId}`);
    });
  });

  describe('taskJobLabel', () => {
    const taskId = 'cb96190b-7f15-4707-990a-c4677ab45ded';
    it('should show the name with the short task ID', () => {
      expect(utils.taskJobLabel(JobType.SHELL, 'multiview_3090_0slot_48c', taskId)).toEqual(
        'multiview_3090_0slot_48c (cb96190b)',
      );
    });
    it('should fall back to the type and the short task ID without a name', () => {
      expect(utils.taskJobLabel(JobType.NOTEBOOK, '', taskId)).toEqual('JupyterLab cb96190b');
    });
  });

  describe('jobTypeToCommandType', () => {
    it('should convert notebook to jupyterlab', () => {
      expect(utils.jobTypeToCommandType(JobType.NOTEBOOK)).toEqual(CommandType.JupyterLab);
    });
    it('should return undefined for non command types', () => {
      expect(utils.jobTypeToCommandType(JobType.EXPERIMENT)).toBeUndefined();
    });
  });

  describe('deviceIdsText', () => {
    it('writes runs of three or more as ranges', () => {
      expect(utils.deviceIdsText([0, 1, 2, 3, 5, 6, 7])).toEqual('0-3, 5-7');
    });
    it('lists pairs and gaps one by one', () => {
      expect(utils.deviceIdsText([0, 1, 5, 6])).toEqual('0, 1, 5, 6');
      expect(utils.deviceIdsText([0, 1, 3, 4, 6, 7])).toEqual('0, 1, 3, 4, 6, 7');
      expect(utils.deviceIdsText([2])).toEqual('2');
      expect(utils.deviceIdsText([])).toEqual('');
    });
    it('sorts the IDs and lists each once', () => {
      expect(utils.deviceIdsText([7, 3, 5, 6, 3, 10, 4])).toEqual('3-7, 10');
    });
  });

  describe('placementLines', () => {
    it('gives one line per agent, by agent ID', () => {
      expect(
        utils.placementLines([
          { agentId: 'node04', deviceIds: [0, 1, 2, 3, 4, 5, 6, 7] },
          { agentId: 'node03', deviceIds: [0, 1, 2, 3, 4, 5, 6, 7] },
        ]),
      ).toEqual(['node03: 0-7', 'node04: 0-7']);
    });
    it('leaves out agents without a slot, and has no line without a placement', () => {
      expect(utils.placementLines([{ agentId: 'node01', deviceIds: [] }])).toEqual([]);
      expect(utils.placementLines(undefined)).toEqual([]);
    });
  });

  describe('poolListsJobGpus', () => {
    const pool = (schedulerType: Api.V1SchedulerType, slotType: ResourceType) =>
      ({ schedulerType, slotType }) as ResourcePool;
    it('is true for an agent pool with GPU slots', () => {
      expect(utils.poolListsJobGpus(pool(Api.V1SchedulerType.PRIORITY, ResourceType.CUDA))).toBe(
        true,
      );
      expect(utils.poolListsJobGpus(pool(Api.V1SchedulerType.FAIRSHARE, ResourceType.ROCM))).toBe(
        true,
      );
    });
    it('is false for Kubernetes, CPU slots, and a pool without an agent', () => {
      expect(utils.poolListsJobGpus(pool(Api.V1SchedulerType.KUBERNETES, ResourceType.CUDA))).toBe(
        false,
      );
      expect(utils.poolListsJobGpus(pool(Api.V1SchedulerType.PRIORITY, ResourceType.CPU))).toBe(
        false,
      );
      expect(
        utils.poolListsJobGpus(pool(Api.V1SchedulerType.PRIORITY, ResourceType.UNSPECIFIED)),
      ).toBe(false);
    });
  });
});
