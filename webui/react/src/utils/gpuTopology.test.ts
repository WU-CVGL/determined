import { getStateColorCssVar } from 'hew/Theme';

import { GPU_TOPOLOGY_CASES, GpuTopologyCase, gpuTopologyCase } from 'fixtures/gpuTopologyCases';
import { V1GpuHealth } from 'services/api-ts-sdk';
import { Resource, ResourceState, ResourceType, SlotState } from 'types';

import {
  gpuHealthSummary,
  gpuHealthWord,
  gpuTopologySummary,
  linkAtStartText,
  numaGroups,
  nvmlErrorsText,
  pairLevels,
  shortPciBusId,
  slotFillColor,
  slotFillOnColor,
  slotFillState,
  switchGroups,
} from './gpuTopology';

const resource = (container?: Resource['container']): Resource => ({
  container,
  enabled: true,
  id: '0',
  name: 'GPU',
  type: ResourceType.CUDA,
});

describe('gpuTopology', () => {
  describe('summaries match the CLI (shared fixture)', () => {
    it('has every shared case', () => {
      expect(GPU_TOPOLOGY_CASES.length).toBeGreaterThanOrEqual(15);
    });

    it.each(GPU_TOPOLOGY_CASES.map((c) => [c.name, c]))('%s', (_, c) => {
      const topo = (c as GpuTopologyCase).gpuTopology ?? undefined;
      expect(gpuTopologySummary(topo)).toBe((c as GpuTopologyCase).topology);
      expect(gpuHealthSummary(topo)).toBe((c as GpuTopologyCase).health);
    });

    it.each([
      ['no-p2p:', '3 PHB no-p2p(TOPOLOGY_NOT_SUPPORTED) (1 unknown)'],
      // Each of the four positions of the lowest NOT_USABLE pair holds a different status, so
      // another status order, or a reversed pair read without swapping, names another status.
      ['no-p2p order:', '3 PHB no-p2p(TOPOLOGY_NOT_SUPPORTED)'],
    ])('keeps the P2P status order when links are reversed and reordered: %s', (name, summary) => {
      const topo = gpuTopologyCase(name);
      expect(gpuTopologySummary(topo)).toBe(summary);
      topo.links = topo.links.reverse().map((l) => ({
        ...l,
        deviceA: l.deviceB,
        deviceB: l.deviceA,
        p2pAToB: l.p2pBToA,
        p2pBToA: l.p2pAToB,
        uuidA: l.uuidB,
        uuidB: l.uuidA,
      }));
      expect(gpuTopologySummary(topo)).toBe(summary);
    });
  });

  describe('slot state to tile fill', () => {
    it('maps the container state', () => {
      expect(slotFillState(undefined)).toBe(SlotState.Free);
      expect(slotFillState(resource())).toBe(SlotState.Free);
      expect(slotFillState(resource({ id: 'c', state: ResourceState.Terminated }))).toBe(
        SlotState.Free,
      );
      expect(slotFillState(resource({ id: 'c', state: ResourceState.Running }))).toBe(
        SlotState.Running,
      );
      [
        ResourceState.Assigned,
        ResourceState.Pulling,
        ResourceState.Starting,
        ResourceState.Warm,
        ResourceState.Unspecified,
        undefined as unknown as ResourceState,
      ].forEach((state) => {
        expect(slotFillState(resource({ id: 'c', state }))).toBe(SlotState.Pending);
      });
    });

    it('uses the SlotAllocationBar palette', () => {
      expect(slotFillColor(SlotState.Running)).toBe(getStateColorCssVar(SlotState.Running));
      expect(slotFillColor(SlotState.Running)).toBe('var(--theme-status-active)');
      expect(slotFillColor(SlotState.Pending)).toBe(getStateColorCssVar(SlotState.Pending));
      expect(slotFillColor(SlotState.Pending)).toBe('var(--theme-status-pending)');
      // The kit has no free colour: the fill falls back to the bar's track colour.
      expect(getStateColorCssVar(SlotState.Free)).toBe('var(--theme-status-free)');
      expect(slotFillColor(SlotState.Free)).toBe(
        'var(--theme-status-free, var(--theme-stage-strong))',
      );
      expect(slotFillOnColor(SlotState.Running)).toBe('var(--theme-status-active-on)');
      expect(slotFillOnColor(SlotState.Free)).toBe('var(--theme-surface-on)');
    });
  });

  describe('grouping', () => {
    it('groups by NUMA node, unknown last', () => {
      const topo = gpuTopologyCase('NUMA unknown');
      const groups = numaGroups(topo.gpus);
      expect(groups.map((g) => g.numaNode)).toEqual([0, -1]);
      expect(groups[0].gpus.map((g) => g.deviceId)).toEqual([0, 1]);
      expect(groups[1].gpus.map((g) => g.deviceId)).toEqual([2, -1]);
    });

    it('groups GPUs behind a PCIe switch', () => {
      const g292 = gpuTopologyCase('g292');
      const groups = switchGroups(g292, g292.gpus);
      expect(groups.map((g) => g.map((gpu) => gpu.deviceId))).toEqual([
        [0, 1],
        [2, 3],
        [4, 5],
        [6, 7],
      ]);
      expect(pairLevels(g292, groups[0])).toEqual(['PIX']);
      expect(pairLevels(g292, g292.gpus)).toEqual(['PIX', 'NODE']);

      const node02 = gpuTopologyCase('node02');
      expect(switchGroups(node02, node02.gpus).every((g) => g.length === 1)).toBe(true);
    });

    it('chains PXB links into one switch group', () => {
      const topo = gpuTopologyCase('every pair unknown');
      const groups = switchGroups(topo, topo.gpus);
      // 0-1, 1-2 and 3-4 are PXB; 2-3 is missing from the report.
      expect(groups.map((g) => g.map((gpu) => gpu.deviceId))).toEqual([
        [0, 1, 2],
        [3, 4],
      ]);
    });
  });

  describe('health facts', () => {
    it('maps the master health to the CLI word', () => {
      expect(gpuHealthWord(V1GpuHealth.OK)).toBe('ok');
      expect(gpuHealthWord(V1GpuHealth.LINKBELOWMAX)).toBe('narrow');
      expect(gpuHealthWord(V1GpuHealth.ERROR)).toBe('error');
      expect(gpuHealthWord(V1GpuHealth.UNSPECIFIED)).toBe('unknown');
      expect(gpuHealthWord(undefined)).toBe('unknown');
    });

    it('describes the link and the NVML errors at agent start', () => {
      const topo = gpuTopologyCase('every pair unknown');
      expect(linkAtStartText(topo.gpus[1])).toBe(
        'x8 of x16, Gen1 of Gen4 (an observation, not a confirmed fault)',
      );
      expect(linkAtStartText(topo.gpus[0])).toBe('unknown');
      expect(linkAtStartText(topo.gpus[3])).toBe(
        'x? of x16, Gen4 of Gen4 (an observation, not a confirmed fault)',
      );
      expect(nvmlErrorsText(topo, topo.gpus[0])).toBe('GetPciInfo: ERROR_GPU_IS_LOST (15)');
      expect(nvmlErrorsText(topo, topo.gpus[1])).toBe('none');
      const unknown = gpuTopologyCase('NVML init failed');
      expect(nvmlErrorsText(unknown, unknown.gpus[0])).toBe('not collected');
    });

    it('shortens bus ids in domain 0000 only', () => {
      expect(shortPciBusId('0000:81:00.0')).toBe('81:00.0');
      expect(shortPciBusId('0001:81:00.0')).toBe('0001:81:00.0');
    });
  });
});
