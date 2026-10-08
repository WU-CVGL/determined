import { IconName } from 'hew/Icon';

import * as Api from 'services/api-ts-sdk';
import { CommandType, Job, JobType, ResourcePool, ResourceType } from 'types';
import { capitalize } from 'utils/string';

export const jobTypeIconName = (jobType: JobType): IconName => {
  if (jobType === JobType.EXTERNAL) {
    return 'external';
  }
  if (jobType === JobType.GENERIC) {
    return 'tasks';
  }
  const type = jobTypeToCommandType(jobType);
  return type ?? 'experiment';
};

export const jobTypeLabel = (jobType: JobType): string => {
  switch (jobType) {
    case JobType.GENERIC:
      return 'Generic Task';
    case JobType.NOTEBOOK:
      return 'JupyterLab';
    case JobType.TENSORBOARD:
      return 'TensorBoard';
    default:
      return capitalize(jobTypeIconName(jobType));
  }
};

/*
 * The job name of a generic task with its short task ID. The master names a task without a name
 * "Generic Task <task ID>", which already contains the ID.
 */
export const genericJobLabel = (name: string, taskId: string): string => {
  if (!name || name === 'Generic Task') return `Generic Task ${taskId}`;
  if (name.includes(taskId)) return name;
  return `${name} (${taskId.split('-')[0]})`;
};

/*
 * The job name of a notebook, shell, command or TensorBoard with its short task ID, as the
 * Tasks page shows it. Tasks without a name keep the type and the short ID.
 */
export const taskJobLabel = (jobType: JobType, name: string, taskId: string): string => {
  const shortId = taskId.split('-')[0];
  if (!name) return `${jobTypeLabel(jobType)} ${shortId}`;
  return `${name} (${shortId})`;
};

// translate JobType to CommandType
export const jobTypeToCommandType = (jobType: JobType): CommandType | undefined => {
  switch (jobType) {
    case JobType.NOTEBOOK:
      return CommandType.JupyterLab;
    case JobType.SHELL:
      return CommandType.Shell;
    case JobType.TENSORBOARD:
      return CommandType.TensorBoard;
    case JobType.COMMAND:
      return CommandType.Command;
    default:
      return undefined;
  }
};

export const orderedSchedulers = new Set<Api.V1SchedulerType>([
  Api.V1SchedulerType.PRIORITY,
  Api.V1SchedulerType.KUBERNETES,
]);

/*
We cannot modify scheduling parameters of non fault tolerant jobs in Kubernetes.
*/
export const canManageJob = (job: Job, rp?: ResourcePool): boolean => {
  if (!rp) return false;
  return !(rp.schedulerType === Api.V1SchedulerType.KUBERNETES && job.type !== JobType.EXPERIMENT);
};

/** A pool whose Active tab lists the GPUs each job holds: an agent pool with GPU slots. */
export const poolListsJobGpus = (rp: ResourcePool): boolean =>
  (rp.schedulerType === Api.V1SchedulerType.PRIORITY ||
    rp.schedulerType === Api.V1SchedulerType.FAIRSHARE) &&
  (rp.slotType === ResourceType.CUDA || rp.slotType === ResourceType.ROCM);

/** Device IDs ascending, each once, with runs of three or more as `a-b`: `0-3, 5, 6`. */
export const deviceIdsText = (ids: number[]): string => {
  const sorted = [...new Set(ids)].sort((a, b) => a - b);
  const parts: string[] = [];
  let start = 0;
  while (start < sorted.length) {
    let end = start;
    while (end + 1 < sorted.length && sorted[end + 1] === sorted[end] + 1) end++;
    if (end - start >= 2) {
      parts.push(`${sorted[start]}-${sorted[end]}`);
    } else {
      parts.push(...sorted.slice(start, end + 1).map(String));
    }
    start = end + 1;
  }
  return parts.join(', ');
};

/** One line per agent where the job holds a slot, by agent ID: `node01: 0-3, 5`. */
export const placementLines = (placement?: Api.V1JobPlacement[]): string[] =>
  (placement ?? [])
    .filter(({ deviceIds }) => deviceIds.length > 0)
    .sort((a, b) => (a.agentId < b.agentId ? -1 : a.agentId > b.agentId ? 1 : 0))
    .map(({ agentId, deviceIds }) => `${agentId}: ${deviceIdsText(deviceIds)}`);
