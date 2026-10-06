import { getStateColorCssVar } from 'hew/Theme';

import {
  V1GpuHealth,
  V1GpuInfo,
  V1GpuLink,
  V1GpuLinkLevel,
  V1GpuP2p,
  V1GpuP2pStatus,
  V1GpuTopology,
} from 'services/api-ts-sdk';
import { Agent, Resource, ResourceState, SlotState } from 'types';

/**
 * Pure helpers of the GPU topology panel. The summary strings are the ones of `det agent list`
 * (harness/determined/cli/agent.py); both are tested against harness/tests/fixtures/
 * gpu_topology_cases.json, so the CLI and the WebUI never disagree.
 */

/** Link levels from best to worst, as NVML's GetTopologyCommonAncestor reports them. */
export const GPU_LINK_LEVELS = ['INTERNAL', 'PIX', 'PXB', 'PHB', 'NODE', 'SYS'] as const;

/** Short P2P status codes, as nvidia-smi topo -p2p prints them where it can. */
export const GPU_P2P_STATUS_CODES: Record<V1GpuP2pStatus, string> = {
  [V1GpuP2pStatus.UNSPECIFIED]: '?',
  [V1GpuP2pStatus.OK]: 'OK',
  [V1GpuP2pStatus.CHIPSETNOTSUPPORTED]: 'CNS',
  [V1GpuP2pStatus.GPUNOTSUPPORTED]: 'GNS',
  [V1GpuP2pStatus.TOPOLOGYNOTSUPPORTED]: 'TNS',
  [V1GpuP2pStatus.DISABLEDBYREGKEY]: 'DIS',
  [V1GpuP2pStatus.NOTSUPPORTED]: 'NS',
};
export const GPU_P2P_STATUS_LEGEND =
  'CNS chipset not supported, GNS GPU not supported, TNS topology not supported, ' +
  'DIS disabled by registry key, NS not supported, ? unknown';

/** A GPU's health as the master classified it, in the words of the CLI. */
export type GpuHealthWord = 'ok' | 'narrow' | 'error' | 'unknown';

/** The words of the health dot and the legend. */
export const GPU_HEALTH_LABELS: Record<GpuHealthWord, string> = {
  error: 'error',
  narrow: 'link below max',
  ok: 'ok',
  unknown: 'unknown',
};

/**
 * Shown with a narrow link. The width is the one measured at agent start; the agent docs explain
 * what it does and does not mean.
 */
export const GPU_NARROW_LINK_TEXT = "A lower link width lowers this link's bandwidth cap.";
export const GPU_EXCLUDED_TEXT = "Left out by the agent's exclude list. No task runs on this GPU.";

export const gpuHealthWord = (health?: V1GpuHealth): GpuHealthWord => {
  switch (health) {
    case V1GpuHealth.OK:
      return 'ok';
    case V1GpuHealth.LINKBELOWMAX:
      return 'narrow';
    case V1GpuHealth.ERROR:
      return 'error';
    default:
      return 'unknown';
  }
};

/** The level without its enum prefix, for example "NODE", or "" when unknown. */
export const linkLevelName = (level?: V1GpuLinkLevel): string => {
  if (!level || level === V1GpuLinkLevel.UNSPECIFIED) return '';
  return level.replace(/^GPU_LINK_LEVEL_/, '');
};

/** The status without its enum prefix, for example "GPU_NOT_SUPPORTED", or "" when unknown. */
export const p2pStatusName = (status?: V1GpuP2pStatus): string => {
  if (!status || status === V1GpuP2pStatus.UNSPECIFIED) return '';
  return status.replace(/^GPU_P2P_STATUS_/, '');
};

/** A bus id without the PCI domain 0000, for example "81:00.0" for "0000:81:00.0". */
export const shortPciBusId = (busId: string): string =>
  busId.startsWith('0000:') ? busId.slice('0000:'.length) : busId;

/** How an excluded GPU is named: its short bus id, or its UUID when the bus id is unknown. */
export const gpuLabel = (gpu: V1GpuInfo): string =>
  gpu.pciBusId ? shortPciBusId(gpu.pciBusId) : gpu.uuid;

/** The slots by device id. */
export const gpuSlots = (topo: V1GpuTopology): V1GpuInfo[] =>
  topo.gpus.filter((g) => !g.excluded).sort((a, b) => a.deviceId - b.deviceId);

/** The slots by device id, then the excluded GPUs in the API's order. */
export const gpuDisplayOrder = (topo: V1GpuTopology): V1GpuInfo[] => [
  ...gpuSlots(topo),
  ...topo.gpus.filter((g) => g.excluded),
];

export interface GpuPairLink {
  link: V1GpuLink;
  /** Whether the first GPU of the lookup is end A of the link. */
  firstIsA: boolean;
}

/** Looks up the link of two GPUs by UUID, in either order. */
export const gpuLinkLookup = (
  topo: V1GpuTopology,
): ((a: V1GpuInfo, b: V1GpuInfo) => GpuPairLink | undefined) => {
  const links = new Map<string, V1GpuLink>();
  topo.links.forEach((link) => links.set(`${link.uuidA}\n${link.uuidB}`, link));
  return (a, b) => {
    const forward = links.get(`${a.uuid}\n${b.uuid}`);
    if (forward) return { firstIsA: true, link: forward };
    const backward = links.get(`${b.uuid}\n${a.uuid}`);
    if (backward) return { firstIsA: false, link: backward };
    return undefined;
  };
};

/**
 * The first known status other than OK, in the order A->B READ, A->B WRITE, B->A READ,
 * B->A WRITE, where A is the GPU the lookup started from.
 */
export const firstNotOkStatus = ({ link, firstIsA }: GpuPairLink): V1GpuP2pStatus | undefined => {
  const [forward, backward] = firstIsA
    ? [link.p2pAToB, link.p2pBToA]
    : [link.p2pBToA, link.p2pAToB];
  return [forward?.read, forward?.write, backward?.read, backward?.write].find(
    (s) => !!s && s !== V1GpuP2pStatus.OK && s !== V1GpuP2pStatus.UNSPECIFIED,
  );
};

const pairs = <T>(items: T[]): [T, T][] =>
  items.flatMap((a, i) => items.slice(i + 1).map((b): [T, T] => [a, b]));

/** NUMA node ids of GPUs in order: known nodes ascending, then unknown (-1). */
const numaOrder = (numas: number[]): number[] =>
  [...new Set(numas)].sort((a, b) => (a < 0 ? 1 : 0) - (b < 0 ? 1 : 0) || a - b);

/**
 * The GPU Topology column of `det agent list`: the NUMA group sizes of the slots, the distinct
 * levels between slots from best to worst, and the P2P state of the pairs of slots, for example
 * "4+4 NODE/SYS p2p 12/28 (3 unknown)". Excluded GPUs do not count.
 */
export const gpuTopologySummary = (topo?: V1GpuTopology): string => {
  if (!topo) return '';
  if (topo.unknownReason) return `unknown: ${topo.unknownReason}`;
  const slots = gpuSlots(topo);
  if (slots.length === 0) return 'no slots';

  const parts = [
    numaOrder(slots.map((g) => g.numaNode))
      .map((n) => slots.filter((g) => g.numaNode === n).length)
      .join('+'),
  ];

  const lookup = gpuLinkLookup(topo);
  const slotPairs = pairs(slots).map(([a, b]) => lookup(a, b));
  const levels = new Set(slotPairs.map((p) => linkLevelName(p?.link.level)));
  const knownLevels = GPU_LINK_LEVELS.filter((l) => levels.has(l));
  if (knownLevels.length > 0) parts.push(knownLevels.join('/'));
  if (slotPairs.length === 0) return parts.join(' ');

  let usable = 0;
  let notUsable = 0;
  let unknown = 0;
  let firstStatus = '';
  slotPairs.forEach((p) => {
    if (p?.link.p2p === V1GpuP2p.USABLE) {
      usable += 1;
    } else if (p?.link.p2p === V1GpuP2p.NOTUSABLE) {
      notUsable += 1;
      if (!firstStatus) firstStatus = p2pStatusName(firstNotOkStatus(p)) || 'unknown';
    } else {
      unknown += 1;
    }
  });

  const total = slotPairs.length;
  let p2p: string;
  if (usable === total) p2p = 'p2p';
  else if (unknown === total) p2p = 'p2p?';
  else if (usable === 0 && notUsable > 0) p2p = `no-p2p(${firstStatus})`;
  else p2p = `p2p ${usable}/${total}`;
  if (unknown > 0 && unknown < total) p2p += ` (${unknown} unknown)`;
  parts.push(p2p);
  return parts.join(' ');
};

/**
 * The GPU Health column of `det agent list`: "ok" when every GPU is ok and none is excluded.
 * Otherwise the slots that are not ok, grouped as error, narrow and unknown, then the excluded
 * GPUs with their state when it is not ok, for example
 * "narrow: 3,5 (x8 of x16); excluded: 81:00.0". The widths are the ones measured at agent start.
 */
export const gpuHealthSummary = (topo?: V1GpuTopology): string => {
  if (!topo) return '';
  const slots = gpuSlots(topo);
  const excluded = topo.gpus.filter((g) => g.excluded);
  const word = (g: V1GpuInfo) => gpuHealthWord(g.health);
  if (excluded.length === 0 && slots.every((g) => word(g) === 'ok')) return 'ok';

  const parts: string[] = [];
  const withError = slots.filter((g) => word(g) === 'error');
  if (withError.length > 0) parts.push(`error: ${withError.map((g) => g.deviceId).join(',')}`);
  const narrow = new Map<string, number[]>();
  slots
    .filter((g) => word(g) === 'narrow')
    .forEach((g) => {
      const width = `x${g.pcieLinkWidth} of x${g.pcieLinkWidthMax}`;
      narrow.set(width, [...(narrow.get(width) ?? []), g.deviceId]);
    });
  if (narrow.size > 0) {
    const groups = [...narrow.entries()].map(([width, ids]) => `${ids.join(',')} (${width})`);
    parts.push(`narrow: ${groups.join(', ')}`);
  }
  const unknown = slots.filter((g) => word(g) === 'unknown');
  if (unknown.length > 0 && unknown.length === slots.length) parts.push('unknown');
  else if (unknown.length > 0) parts.push(`unknown: ${unknown.map((g) => g.deviceId).join(',')}`);
  if (excluded.length > 0) {
    const labels = excluded.map((g) => gpuLabel(g) + (word(g) === 'ok' ? '' : ` (${word(g)})`));
    parts.push(`excluded: ${labels.join(', ')}`);
  }
  return parts.join('; ');
};

/** The NUMA groups of GPUs: known NUMA nodes ascending, then the GPUs whose node is unknown. */
export const numaGroups = (gpus: V1GpuInfo[]): { numaNode: number; gpus: V1GpuInfo[] }[] =>
  numaOrder(gpus.map((g) => g.numaNode)).map((numaNode) => ({
    gpus: gpus.filter((g) => g.numaNode === numaNode),
    numaNode,
  }));

/**
 * Switch groups: the connected components of PIX links among the given GPUs, in the order of their
 * first GPU. A GPU without a PIX link is a group of its own. PIX means one PCIe switch between two
 * GPUs, so a group is the GPUs behind one switch. PXB (several switches, no host bridge) never joins
 * a group: GPUs behind two switches under a common switch would otherwise show as one switch. It
 * shows in the pairwise matrix only.
 */
export const switchGroups = (topo: V1GpuTopology, gpus: V1GpuInfo[]): V1GpuInfo[][] => {
  const lookup = gpuLinkLookup(topo);
  const parent = gpus.map((_, i) => i);
  const find = (i: number): number => {
    while (parent[i] !== i) i = parent[i];
    return i;
  };
  gpus.forEach((a, i) =>
    gpus.forEach((b, j) => {
      if (j <= i) return;
      if (linkLevelName(lookup(a, b)?.link.level) === 'PIX') parent[find(j)] = find(i);
    }),
  );
  const groups = new Map<number, V1GpuInfo[]>();
  gpus.forEach((g, i) => {
    const root = find(i);
    groups.set(root, [...(groups.get(root) ?? []), g]);
  });
  return [...groups.values()];
};

/** The distinct known levels between the given GPUs, from best to worst. */
export const pairLevels = (topo: V1GpuTopology, gpus: V1GpuInfo[]): string[] => {
  const lookup = gpuLinkLookup(topo);
  const levels = new Set(pairs(gpus).map(([a, b]) => linkLevelName(lookup(a, b)?.link.level)));
  return GPU_LINK_LEVELS.filter((l) => levels.has(l));
};

/**
 * The fill state of a slot's tile, by the slot's container: none or TERMINATED is Free,
 * RUNNING is Running, and every other state (ASSIGNED, PULLING, STARTING, or one the WebUI does
 * not know) is Pending. Unlike SlotAllocationBar, this never counts a slot without a running
 * container as pending.
 */
export const slotFillState = (resource?: Resource): SlotState => {
  const container = resource?.container;
  if (!container || container.state === ResourceState.Terminated) return SlotState.Free;
  if (container.state === ResourceState.Running) return SlotState.Running;
  return SlotState.Pending;
};

/** Why a slot or an agent takes no new work. */
export type OffLabel = 'draining' | 'disabled';

/**
 * Why the whole agent takes no new work: a drain also disables it, so draining comes first. An
 * agent without the fields counts as enabled, as the master's summary defaults to.
 */
export const agentOffLabel = (agent: Pick<Agent, 'draining' | 'enabled'>): OffLabel | undefined => {
  if (agent.draining) return 'draining';
  if (agent.enabled === false) return 'disabled';
  return undefined;
};

/**
 * Why a slot takes no new work, as its stripe label: draining before disabled. The agent's state
 * counts too: the scheduler gives a disabled or draining agent no new work, also when a slot of it
 * was enabled again on its own (`det slot enable`).
 */
export const slotOffLabel = (resource?: Resource, agentOff?: OffLabel): OffLabel | undefined => {
  if (resource?.draining || agentOff === 'draining') return 'draining';
  if ((resource && !resource.enabled) || agentOff === 'disabled') return 'disabled';
  return undefined;
};

/**
 * The count line of an agent's panel. Every slot counts once, by its fill: running, pending or
 * unoccupied, or unknown without a slot record. The unoccupied slots split into allocatable (they
 * can take new work), disabled and draining, by slotOffLabel, so a slot of a disabled or draining
 * agent is never allocatable. A running or pending slot counts as running or pending also when it is
 * disabled or draining; its stripes show that. Excluded GPUs are not slots and come last, for example
 * "7 slots: 1 running, 1 pending, 5 unoccupied (3 allocatable, 1 disabled, 1 draining); 1 excluded".
 */
export const gpuSlotCountText = (
  gpus: V1GpuInfo[],
  resourceOf: (gpu: V1GpuInfo) => Resource | undefined,
  agentOff?: OffLabel,
): string => {
  const slots = gpus.filter((g) => !g.excluded);
  const n = { allocatable: 0, disabled: 0, draining: 0, pending: 0, running: 0, unknown: 0 };
  slots.forEach((g) => {
    const resource = resourceOf(g);
    const fill = slotFillState(resource);
    if (fill === SlotState.Running) n.running += 1;
    else if (fill === SlotState.Pending) n.pending += 1;
    else if (!resource) n.unknown += 1;
    else n[slotOffLabel(resource, agentOff) ?? 'allocatable'] += 1;
  });
  const unoccupied = n.allocatable + n.disabled + n.draining;
  const off = (['disabled', 'draining'] as const)
    .filter((k) => n[k] > 0)
    .map((k) => `, ${n[k]} ${k}`)
    .join('');
  const excluded = gpus.length - slots.length;
  return (
    `${slots.length} slots: ${n.running} running, ${n.pending} pending, ` +
    `${unoccupied} unoccupied (${n.allocatable} allocatable${off})` +
    (n.unknown > 0 ? `, ${n.unknown} unknown` : '') +
    (excluded > 0 ? `; ${excluded} excluded` : '')
  );
};

/**
 * The tile fill colour: the palette of SlotAllocationBar (the slot-state colours of the design
 * kit). The kit has no colour for a free slot, so Free falls back to the bar's track colour.
 */
export const slotFillColor = (state: SlotState): string =>
  state === SlotState.Free
    ? 'var(--theme-status-free, var(--theme-stage-strong))'
    : getStateColorCssVar(state);

/**
 * The tile edge: Running and Pending take their fill colour, as in the approved preview; a Free tile
 * keeps the surface border (undefined).
 */
export const slotFillEdgeColor = (state: SlotState): string | undefined =>
  state === SlotState.Free ? undefined : slotFillColor(state);

/** The text colour on a tile fill. */
export const slotFillOnColor = (state: SlotState): string =>
  state === SlotState.Free ? 'var(--theme-surface-on)' : getStateColorCssVar(state, { isOn: true });

/** "<cur> of <max>" for a link width, with ? for an unknown value (0). */
export const linkValueText = (cur: number, max: number, prefix: string): string => {
  const value = (v: number) => (v > 0 ? `${prefix}${v}` : `${prefix}?`);
  return `${value(cur)} of ${value(max)}`;
};

/**
 * The first fact of a GPU's health: its PCIe link, as measured at agent start. The width as current
 * of max, and the generation as the highest that the GPU and its slot support: the current
 * generation drops while a GPU is idle, so it is left out, as in `det agent describe`.
 */
export const linkText = (gpu: V1GpuInfo): string => {
  const { pcieLinkWidth, pcieLinkWidthMax, pcieLinkGenMax } = gpu;
  if (!pcieLinkWidth && !pcieLinkWidthMax && !pcieLinkGenMax) return 'unknown';
  const gen = pcieLinkGenMax > 0 ? `Gen${pcieLinkGenMax}` : 'Gen?';
  return `${linkValueText(pcieLinkWidth, pcieLinkWidthMax, 'x')}, ${gen}`;
};

/** The second fact of a GPU's health: its NVML errors, as measured at agent start. */
export const nvmlErrorsText = (topo: V1GpuTopology, gpu: V1GpuInfo): string => {
  if (topo.unknownReason) return 'not collected';
  return gpu.nvmlError || 'none';
};
