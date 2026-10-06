import { CommandType, JobType } from 'types';

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
});
