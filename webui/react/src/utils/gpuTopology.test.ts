import dayjs from 'dayjs';
import { getStateColorCssVar } from 'hew/Theme';

import { GPU_TOPOLOGY_CASES, GpuTopologyCase, gpuTopologyCase } from 'fixtures/gpuTopologyCases';
import { V1GpuHealth, V1GpuInfo } from 'services/api-ts-sdk';
import { Resource, ResourceState, ResourceType, SlotState } from 'types';

import {
  agentOffLabel,
  GPU_HEALTH_LABELS,
  GPU_NARROW_LINK_TEXT,
  gpuHealthSummary,
  gpuHealthWord,
  gpuSlotCountText,
  gpuTopologySummary,
  linkText,
  numaGroups,
  nvmlErrorsText,
  pairLevels,
  recentXidTexts,
  shortPciBusId,
  slotFillColor,
  slotFillOnColor,
  slotFillState,
  slotOffLabel,
  switchGroups,
  xidWindowText,
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

    it.each(GPU_TOPOLOGY_CASES.filter((c) => c.pcieLink).map((c) => [c.name, c]))(
      'PCIe link of each GPU: %s',
      (_, c) => {
        const { gpuTopology, pcieLink } = c as GpuTopologyCase;
        expect(gpuTopology?.gpus.map(linkText)).toEqual(pcieLink);
      },
    );

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

  describe('slot counts', () => {
    it('counts each slot once; only enabled, not draining unoccupied slots are allocatable', () => {
      const topo = gpuTopologyCase('node01 with the exclude list');
      const running = { id: 'c', state: ResourceState.Running };
      const resources: Record<number, Resource | undefined> = {
        0: { ...resource(running), draining: true, enabled: false },
        1: { ...resource({ id: 'c', state: ResourceState.Pulling }), enabled: false },
        2: resource({ id: 'c', state: ResourceState.Terminated }),
        3: resource(),
        5: { ...resource(), enabled: false },
        6: { ...resource(), draining: true, enabled: false },
        7: undefined,
      };
      const resourceOf = (g: V1GpuInfo) => (g.excluded ? undefined : resources[g.deviceId]);
      expect(gpuSlotCountText(topo.gpus, resourceOf)).toBe(
        '7 slots: 1 running, 1 pending, 4 unoccupied (2 allocatable, 1 disabled, 1 draining), ' +
          '1 unknown; 1 excluded',
      );
      expect(gpuSlotCountText(topo.gpus, (g) => (g.excluded ? undefined : resource()))).toBe(
        '7 slots: 0 running, 0 pending, 7 unoccupied (7 allocatable); 1 excluded',
      );
    });

    it('reads the agent state: draining first, a missing enabled counts as enabled', () => {
      expect(agentOffLabel({})).toBeUndefined();
      expect(agentOffLabel({ enabled: true })).toBeUndefined();
      expect(agentOffLabel({ enabled: false })).toBe('disabled');
      expect(agentOffLabel({ draining: true, enabled: false })).toBe('draining');
    });

    it('takes no new work on a slot that is off or whose agent is off, draining first', () => {
      const disabled = { ...resource(), enabled: false };
      expect(slotOffLabel(resource())).toBeUndefined();
      expect(slotOffLabel(disabled)).toBe('disabled');
      expect(slotOffLabel({ ...disabled, draining: true })).toBe('draining');
      expect(slotOffLabel(resource(), 'disabled')).toBe('disabled');
      expect(slotOffLabel(resource(), 'draining')).toBe('draining');
      expect(slotOffLabel(disabled, 'draining')).toBe('draining');
      expect(slotOffLabel({ ...disabled, draining: true }, 'disabled')).toBe('draining');
    });

    it('counts no slot of a disabled or draining agent as allocatable', () => {
      // `det slot enable` on a slot of a disabled agent leaves the slot enabled, but the scheduler
      // gives the agent no new work.
      const topo = gpuTopologyCase('node02');
      const running = resource({ id: 'c', state: ResourceState.Running });
      const resourceOf = (g: V1GpuInfo) => (g.deviceId === 0 ? running : resource());
      expect(gpuSlotCountText(topo.gpus, resourceOf, agentOffLabel({ enabled: false }))).toBe(
        '8 slots: 1 running, 0 pending, 7 unoccupied (0 allocatable, 7 disabled)',
      );
      expect(
        gpuSlotCountText(topo.gpus, resourceOf, agentOffLabel({ draining: true, enabled: false })),
      ).toBe('8 slots: 1 running, 0 pending, 7 unoccupied (0 allocatable, 7 draining)');
      expect(gpuSlotCountText(topo.gpus, resourceOf, agentOffLabel({ enabled: true }))).toBe(
        '8 slots: 1 running, 0 pending, 7 unoccupied (7 allocatable)',
      );
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

    it('keeps two switches under a common switch apart', () => {
      // 0-1 and 2-3 are PIX; the four pairs across are PXB.
      const topo = gpuTopologyCase('two PCIe switches');
      const groups = switchGroups(topo, topo.gpus);
      expect(groups.map((g) => g.map((gpu) => gpu.deviceId))).toEqual([
        [0, 1],
        [2, 3],
      ]);
      expect(groups.map((g) => pairLevels(topo, g))).toEqual([['PIX'], ['PIX']]);
      expect(pairLevels(topo, topo.gpus)).toEqual(['PIX', 'PXB']);
    });

    it('never groups by PXB links', () => {
      const topo = gpuTopologyCase('every pair unknown');
      const groups = switchGroups(topo, topo.gpus);
      // 0-1, 1-2 and 3-4 are PXB; 2-3 is missing from the report.
      expect(groups.map((g) => g.map((gpu) => gpu.deviceId))).toEqual([[0], [1], [2], [3], [4]]);
      expect(pairLevels(topo, topo.gpus)).toEqual(['PXB']);
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

    it('names the health states and explains a narrow link in the words of the CLI', () => {
      expect(GPU_HEALTH_LABELS).toEqual({
        error: 'error',
        narrow: 'link below max',
        ok: 'ok',
        unknown: 'unknown',
      });
      expect(GPU_NARROW_LINK_TEXT).toBe("A lower link width lowers this link's bandwidth cap.");
    });

    it('describes the PCIe link and the NVML errors', () => {
      const topo = gpuTopologyCase('every pair unknown');
      // Idle at Gen1 of Gen4: the width as measured, the highest generation only.
      expect(linkText(topo.gpus[1])).toBe('x8 of x16, Gen4');
      // Gen3 with an unknown highest generation.
      expect(linkText(topo.gpus[2])).toBe('x4 of x16, Gen?');
      expect(linkText(topo.gpus[0])).toBe('unknown');
      expect(linkText(topo.gpus[3])).toBe('x? of x16, Gen4');
      // Only the current generation known.
      const genOnly = { ...topo.gpus[0], pcieLinkGen: 1 };
      expect(linkText(genOnly)).toBe('unknown');
      expect(nvmlErrorsText(topo, topo.gpus[0])).toBe('GetPciInfo: ERROR_GPU_IS_LOST (15)');
      expect(nvmlErrorsText(topo, topo.gpus[1])).toBe('none');
      const unknown = gpuTopologyCase('NVML init failed');
      expect(nvmlErrorsText(unknown, unknown.gpus[0])).toBe('not collected');
    });

    it('lists recent critical XIDs with their first and last observed windows', () => {
      const topo = gpuTopologyCase('recent critical XIDs');
      // A 5-minute window by its end, in local time.
      const end = dayjs('2026-10-07T10:10:00Z');
      expect(xidWindowText('2026-10-07T10:10:00Z')).toBe(
        `${end.subtract(5, 'minute').format('YYYY-MM-DD, HH:mm')}–${end.format('HH:mm')}`,
      );
      expect(xidWindowText('2026-10-07T10:10:00Z')).toMatch(
        /^\d{4}-\d{2}-\d{2}, \d{2}:\d{2}–\d{2}:\d{2}$/,
      );
      const w = xidWindowText;
      expect(recentXidTexts(topo.gpus[0])).toEqual([
        `79 (${w('2026-10-07T10:10:00Z')} to ${w('2026-10-07T10:25:00Z')})`,
      ]);
      // By code, as the master sends them; one window when first and last are the same.
      expect(recentXidTexts(topo.gpus[1])).toEqual([
        `48 (${w('2026-10-07T11:00:00Z')})`,
        `79 (${w('2026-10-07T11:00:00Z')} to ${w('2026-10-07T11:05:00Z')})`,
      ]);
      expect(recentXidTexts(topo.gpus[2])).toEqual([]);
      expect(recentXidTexts(topo.gpus[3])).toEqual([`94 (${w('2026-10-07T12:30:00Z')})`]);
      // A master of an earlier version sends none.
      expect(recentXidTexts(gpuTopologyCase('node02').gpus[0])).toEqual([]);
    });

    it('shortens bus ids in domain 0000 only', () => {
      expect(shortPciBusId('0000:81:00.0')).toBe('81:00.0');
      expect(shortPciBusId('0001:81:00.0')).toBe('0001:81:00.0');
    });
  });
});
