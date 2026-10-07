import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { ThemeProvider } from 'components/ThemeProvider';
import { gpuTopologyCase } from 'fixtures/gpuTopologyCases';
import { V1GpuHealth, V1GpuTopology, V1GpuXidQueryStatus } from 'services/api-ts-sdk';
import { Agent, Resource, ResourceState, ResourceType } from 'types';
import { GPU_EXCLUDED_TEXT, GPU_NARROW_LINK_TEXT } from 'utils/gpuTopology';

import ClusterTopology from './ClusterTopology';
import GpuTopology, { GPU_TOPOLOGY_DOCS_PATH, GpuTopologyLegend } from './GpuTopology';

const slot = (id: number, overrides: Partial<Resource> = {}): Resource => ({
  enabled: true,
  id: String(id),
  name: 'NVIDIA GeForce RTX 3090',
  type: ResourceType.CUDA,
  ...overrides,
});

const agentOf = (
  id: string,
  gpuTopology: V1GpuTopology | undefined,
  overrides: Record<number, Partial<Resource>> = {},
  agentOverrides: Partial<Agent> = {},
): Agent => ({
  gpuTopology,
  id,
  registeredTime: 0,
  resourcePools: ['pool'],
  resources: (gpuTopology?.gpus ?? [])
    .filter((g) => !g.excluded)
    .map((g) => slot(g.deviceId, overrides[g.deviceId])),
  slotStats: { brandStats: {}, typeStats: {} },
  ...agentOverrides,
});

const setup = (children: React.ReactNode) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>{children}</ThemeProvider>
    </UIProvider>,
  );

const tile = (name: string) => screen.getByRole('group', { name: new RegExp(`^${name},`) });

/** The antd popover around an element of the details popup. */
const overlayOf = (el: HTMLElement): HTMLElement => {
  const overlay = el.closest<HTMLElement>('.ant-popover');
  if (!overlay) throw new Error('not in a popover');
  return overlay;
};

/** The details popup in a tooltip: the element that a pin turns into the dialog. */
const popupIn = (tooltip: HTMLElement): HTMLElement => {
  const popup = tooltip.querySelector<HTMLElement>('.popup');
  if (!popup) throw new Error('no details popup');
  return popup;
};

describe('GpuTopology', () => {
  it('shows the CLI summaries, health dots, slot fills and stripes', () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'), {
      0: { container: { id: 'c0', state: ResourceState.Running } },
      1: { container: { id: 'c1', state: ResourceState.Pulling } },
      2: { container: { id: 'c2', state: ResourceState.Terminated } },
      5: { enabled: false },
      6: { draining: true, enabled: false },
    });
    setup(<GpuTopology agent={agent} />);

    expect(screen.getByText('4+3 NODE/SYS p2p')).toBeInTheDocument();
    expect(screen.getByText('narrow: 3,5 (x8 of x16); excluded: 81:00.0')).toBeInTheDocument();
    // Running, pending and unoccupied slots; only the enabled, not draining unoccupied slots are
    // allocatable. Then the excluded GPUs.
    expect(
      screen.getByText(
        '7 slots: 1 running, 1 pending, 5 unoccupied (3 allocatable, 1 disabled, 1 draining); ' +
          '1 excluded',
      ),
    ).toBeInTheDocument();

    // One dot per GPU, labelled for screen readers: amber on the x8 slots, green elsewhere.
    expect(screen.getAllByRole('img', { name: 'GPU health: link below max' })).toHaveLength(2);
    expect(screen.getAllByRole('img', { name: 'GPU health: ok' })).toHaveLength(6);
    expect(within(tile('Slot 3')).getByRole('img').getAttribute('aria-label')).toBe(
      'GPU health: link below max',
    );

    // The fill is the slot state; health never changes it.
    expect(tile('Slot 0').dataset.fill).toBe('RUNNING');
    expect(tile('Slot 1').dataset.fill).toBe('PENDING');
    expect(tile('Slot 2').dataset.fill).toBe('FREE');
    expect(tile('Slot 3').dataset.fill).toBe('FREE');
    expect(tile('Slot 0').style.getPropertyValue('--gpu-tile-fill')).toBe(
      'var(--theme-status-active)',
    );
    // Running and Pending tiles take their fill as the edge; a Free tile keeps the surface border.
    expect(tile('Slot 0').style.getPropertyValue('--gpu-tile-edge')).toBe(
      'var(--theme-status-active)',
    );
    expect(tile('Slot 1').style.getPropertyValue('--gpu-tile-edge')).toBe(
      tile('Slot 1').style.getPropertyValue('--gpu-tile-fill'),
    );
    expect(tile('Slot 1').style.getPropertyValue('--gpu-tile-edge')).not.toBe('');
    expect(tile('Slot 2').style.getPropertyValue('--gpu-tile-edge')).toBe('');

    // Disabled and draining slots are striped and labelled, never a fill colour.
    expect(tile('Slot 5')).toHaveClass('striped');
    expect(within(tile('Slot 5')).getByText('disabled')).toBeInTheDocument();
    expect(tile('Slot 6')).toHaveClass('striped');
    expect(within(tile('Slot 6')).getByText('draining')).toBeInTheDocument();
    expect(tile('Slot 6').dataset.fill).toBe('FREE');
    expect(tile('Slot 7')).not.toHaveClass('striped');

    // The excluded GPU: no slot id, Free fill, stripes, its label, a dot and an info button.
    const excluded = tile('Excluded GPU 81:00.0');
    expect(excluded).toHaveClass('striped');
    expect(excluded.querySelector('.tileName')).toHaveTextContent(/^\u2013$/);
    expect(excluded).not.toHaveTextContent('-1');
    expect(excluded.dataset.fill).toBe('FREE');
    expect(within(excluded).getByText('excluded')).toBeInTheDocument();
    expect(within(excluded).getByRole('img', { name: 'GPU health: ok' })).toBeInTheDocument();
    expect(
      within(excluded).getByRole('button', { name: 'Details for excluded GPU 81:00.0 on node01' }),
    ).toBeInTheDocument();

    // NUMA boxes; the excluded GPU sits in its NUMA node.
    const numa1 = screen.getByRole('region', { name: 'NUMA 1' });
    expect(within(numa1).getAllByRole('group')).toHaveLength(4);
  });

  it('names each tile by its fill state, then its stripe label', () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'), {
      0: { container: { id: 'c0', state: ResourceState.Running } },
      1: { container: { id: 'c1', state: ResourceState.Pulling }, enabled: false },
      5: { container: { id: 'c5', state: ResourceState.Running }, enabled: false },
      6: { container: { id: 'c6', state: ResourceState.Running }, draining: true, enabled: false },
      7: { draining: true, enabled: false },
    });
    setup(<GpuTopology agent={agent} />);
    const names = screen.getAllByRole('group').map((g) => g.getAttribute('aria-label'));
    expect(names).toEqual(
      expect.arrayContaining([
        'Slot 0, Running',
        'Slot 1, Pending, disabled',
        'Slot 2, Free',
        'Slot 5, Running, disabled',
        'Slot 6, Running, draining',
        'Slot 7, Free, draining',
        // An excluded GPU has the Free fill for its colour only: its name leaves it out.
        'Excluded GPU 81:00.0, excluded',
      ]),
    );
    expect(
      within(tile('Slot 6')).getByRole('button', { name: 'Details for slot 6 on node01' }),
    ).toBeInTheDocument();
  });

  it('shows the details on hover and pins the same popup in place on click', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    const button = within(tile('Slot 3')).getByRole('button');

    await userEvent.hover(button);
    const tooltip = await screen.findByRole('tooltip');
    expect(button).toHaveAttribute('aria-describedby', tooltip.id);
    expect(tooltip).toHaveTextContent('Slot 3 on node01: link below max');
    expect(tooltip).toHaveTextContent('UUID');
    expect(tooltip).toHaveTextContent('0000:61:00.0');
    expect(tooltip).toHaveTextContent(/PCIe linkx8 of x16, Gen4NVML errors/);
    // Only the time: the details end with it, before the narrow link text.
    expect(tooltip).toHaveTextContent(
      /NVML errorsnoneCollected at\d{4}-\d{2}-\d{2}, \d{2}:\d{2}:\d{2}A lower link width/,
    );
    expect(tooltip).toHaveTextContent("A lower link width lowers this link's bandwidth cap.");
    expect(tooltip).not.toHaveTextContent(
      /XID|agent start|agent clock|observation|confirmed|collective/,
    );
    // The popup a pin keeps, without an arrow and in the theme of the page. It has no close button
    // and takes no pointer events. (jsdom does not align popups, so antd adds no placement class.)
    const popup = popupIn(tooltip);
    const overlay = overlayOf(tooltip);
    expect(overlay.querySelector('.ant-popover-arrow')).toBeNull();
    expect(overlay.className).toMatch(/\bui-provider-/);
    expect(overlay).toHaveStyle({ pointerEvents: 'none' });
    expect(within(popup).queryByRole('button')).toBeNull();
    expect(screen.queryByRole('dialog')).toBeNull();

    // A click while the popup shows pins it: the same element in the same place, now a dialog
    // with a close button that takes pointer events.
    await userEvent.click(button);
    const dialog = await screen.findByRole('dialog', { name: 'Details for slot 3 on node01' });
    expect(dialog).toBe(popup);
    expect(overlayOf(dialog)).toBe(overlay);
    expect(document.querySelectorAll('.ant-popover')).toHaveLength(1);
    expect(overlay).not.toHaveClass('ant-popover-hidden');
    expect(overlay).not.toHaveStyle({ pointerEvents: 'none' });
    expect(within(dialog).getByRole('button', { name: 'Close details' })).toBeInTheDocument();
    expect(button).toHaveAttribute('aria-expanded', 'true');
    expect(button).not.toHaveAttribute('aria-describedby');
    // A pointer pin leaves focus where the click put it.
    expect(button).toHaveFocus();
    expect(dialog).toHaveTextContent(GPU_NARROW_LINK_TEXT);
    const docs = within(dialog).getByRole('link', { name: 'GPU topology and health' });
    expect(docs.getAttribute('href')).toContain(GPU_TOPOLOGY_DOCS_PATH);

    // Pinned, it stays when the pointer leaves.
    await userEvent.unhover(button);
    expect(screen.getByRole('dialog')).toBe(popup);

    // A second click closes it.
    await userEvent.hover(button);
    await userEvent.click(button);
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('dialog')).toBeNull();
    await waitFor(() => expect(overlay).toHaveClass('ant-popover-hidden'));
  });

  it('lets clicks through the hover popup and hides it when the pointer leaves', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    const button = within(tile('Slot 3')).getByRole('button');

    await userEvent.hover(button);
    const tooltip = await screen.findByRole('tooltip');
    // The link inherits the popup's pointer-events: none, so user-event refuses to click it.
    await expect(
      userEvent.click(within(tooltip).getByRole('link', { name: 'GPU topology and health' })),
    ).rejects.toThrow(/pointer-events/);

    await userEvent.hover(button);
    await userEvent.unhover(button);
    await waitFor(() => expect(overlayOf(tooltip)).toHaveClass('ant-popover-hidden'));
    expect(button).not.toHaveAttribute('aria-describedby');
    expect(button).toHaveAttribute('aria-expanded', 'false');
  });

  it('closes the pinned details with their close button and gives focus back', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    const button = within(tile('Slot 3')).getByRole('button');

    await userEvent.click(button);
    const dialog = await screen.findByRole('dialog', { name: 'Details for slot 3 on node01' });
    expect(button).toHaveAttribute('aria-expanded', 'true');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Close details' }));
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(button).toHaveFocus();
    // The focus back on the button does not show the details again.
    await waitFor(() => expect(overlayOf(dialog)).toHaveClass('ant-popover-hidden'));
    expect(button).not.toHaveAttribute('aria-describedby');
  });

  it('closes the pinned details on a click outside them, not on one inside', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    const button = within(tile('Slot 3')).getByRole('button');

    await userEvent.click(button);
    const dialog = await screen.findByRole('dialog', { name: 'Details for slot 3 on node01' });
    await userEvent.click(within(dialog).getByText('UUID'));
    expect(button).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('dialog')).toBe(dialog);

    await userEvent.click(within(tile('Slot 5')).getByText('5'));
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('dialog')).toBeNull();
    await waitFor(() => expect(overlayOf(dialog)).toHaveClass('ant-popover-hidden'));
  });

  it('counts a slot that is running and disabled as running, never as allocatable', () => {
    const agent = agentOf('node02', gpuTopologyCase('node02'), {
      0: { container: { id: 'c0', state: ResourceState.Running }, enabled: false },
      5: { enabled: false },
    });
    setup(<GpuTopology agent={agent} />);
    expect(
      screen.getByText('8 slots: 1 running, 0 pending, 7 unoccupied (6 allocatable, 1 disabled)'),
    ).toBeInTheDocument();
    // The stripes still mark the running slot as disabled.
    expect(tile('Slot 0')).toHaveClass('striped');
  });

  it('counts no slot of a disabled or draining agent as allocatable and stripes its tiles', () => {
    // `det slot enable` on a slot of a disabled agent leaves the slot enabled, but the scheduler
    // gives the agent no new work.
    const agent = agentOf(
      'node02',
      gpuTopologyCase('node02'),
      { 0: { container: { id: 'c0', state: ResourceState.Running } } },
      { enabled: false },
    );
    const { unmount } = setup(<GpuTopology agent={agent} />);
    expect(
      screen.getByText('8 slots: 1 running, 0 pending, 7 unoccupied (0 allocatable, 7 disabled)'),
    ).toBeInTheDocument();
    expect(tile('Slot 0').getAttribute('aria-label')).toBe('Slot 0, Running, disabled');
    expect(tile('Slot 3').getAttribute('aria-label')).toBe('Slot 3, Free, disabled');
    expect(tile('Slot 3')).toHaveClass('striped');
    unmount();

    setup(<GpuTopology agent={{ ...agent, draining: true }} />);
    expect(
      screen.getByText('8 slots: 1 running, 0 pending, 7 unoccupied (0 allocatable, 7 draining)'),
    ).toBeInTheDocument();
    expect(within(tile('Slot 3')).getByText('draining')).toBeInTheDocument();
  });

  it('names a slot without a slot record unknown, as the count line does', async () => {
    const base = agentOf('node02', gpuTopologyCase('node02'));
    setup(
      <GpuTopology agent={{ ...base, resources: base.resources.filter((r) => r.id !== '7') }} />,
    );
    expect(
      screen.getByText('8 slots: 0 running, 0 pending, 7 unoccupied (7 allocatable), 1 unknown'),
    ).toBeInTheDocument();
    expect(tile('Slot 7').getAttribute('aria-label')).toBe('Slot 7, Unknown');
    expect(within(tile('Slot 7')).getByText('Unknown')).toBeInTheDocument();
    await userEvent.hover(within(tile('Slot 7')).getByRole('button'));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('StateUnknown (no slot record)');
  });

  it('shows the details on focus, pins them on a click, and closes them on Escape', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    const button = within(tile('Slot 3')).getByRole('button');

    act(() => button.focus());
    const tooltip = await screen.findByRole('tooltip');
    expect(button).toHaveAttribute('aria-describedby', tooltip.id);
    expect(tooltip).toHaveTextContent('Slot 3 on node01: link below max');
    expect(tooltip).toHaveTextContent(GPU_NARROW_LINK_TEXT);
    const popup = popupIn(tooltip);
    expect(overlayOf(tooltip)).toHaveStyle({ pointerEvents: 'none' });

    // Pinned, the popup stays the same element.
    await userEvent.click(button);
    expect(await screen.findByRole('dialog')).toBe(popup);
    expect(document.querySelectorAll('.ant-popover')).toHaveLength(1);

    await userEvent.keyboard('{Escape}');
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(button).toHaveFocus();
    await waitFor(() => expect(overlayOf(tooltip)).toHaveClass('ant-popover-hidden'));

    // Escape also hides the details that focus shows.
    act(() => button.blur());
    act(() => button.focus());
    await waitFor(() => expect(overlayOf(tooltip)).not.toHaveClass('ant-popover-hidden'));
    await userEvent.keyboard('{Escape}');
    await waitFor(() => expect(overlayOf(tooltip)).toHaveClass('ant-popover-hidden'));
    expect(button).toHaveFocus();
  });

  it('moves focus into the details pinned from the keyboard and back on Escape', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    const button = within(tile('Slot 3')).getByRole('button');
    act(() => button.focus());
    const popup = popupIn(await screen.findByRole('tooltip'));

    // The popup is portalled to the end of the page: Tab must reach its close button and its docs
    // link next.
    await userEvent.keyboard('{Enter}');
    const dialog = await screen.findByRole('dialog', { name: 'Details for slot 3 on node01' });
    expect(dialog).toBe(popup);
    expect(button).toHaveAttribute('aria-expanded', 'true');
    await waitFor(() => expect(dialog).toHaveFocus());
    await userEvent.tab();
    expect(within(dialog).getByRole('button', { name: 'Close details' })).toHaveFocus();
    await userEvent.tab();
    expect(within(dialog).getByRole('link', { name: 'GPU topology and health' })).toHaveFocus();

    await userEvent.keyboard('{Escape}');
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(button).toHaveFocus();
    await waitFor(() => expect(overlayOf(dialog)).toHaveClass('ant-popover-hidden'));
  });

  it('explains an excluded GPU', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    await userEvent.click(within(tile('Excluded GPU 81:00.0')).getByRole('button'));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent(GPU_EXCLUDED_TEXT);
    expect(dialog).toHaveTextContent('Slotnone');
    expect(dialog).toHaveTextContent('StateExcluded');
    expect(dialog).not.toHaveTextContent(GPU_NARROW_LINK_TEXT);
  });

  it('lists recent critical XIDs in the details only when there are any', async () => {
    const topo = gpuTopologyCase('recent critical XIDs');
    setup(<GpuTopology agent={agentOf('a', topo)} />);
    // A GPU with a recent critical XID has the error dot, also on a narrow link.
    expect(
      within(tile('Slot 1')).getByRole('img', { name: 'GPU health: error' }),
    ).toBeInTheDocument();
    await userEvent.click(within(tile('Slot 1')).getByRole('button'));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('Slot 1 on a: error');
    expect(dialog).toHaveTextContent(
      /Collected at\d{4}-\d{2}-\d{2}, \d{2}:\d{2}:\d{2}Recent critical XIDs48 \(.+\)79 \(.+ to .+\)$/,
    );
    expect(dialog).not.toHaveTextContent(GPU_NARROW_LINK_TEXT);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Close details' }));

    await userEvent.click(within(tile('Slot 2')).getByRole('button'));
    const narrow = await screen.findByRole('dialog');
    expect(narrow).toHaveTextContent('Slot 2 on a: link below max');
    expect(narrow).not.toHaveTextContent(/XID/);
  });

  it.each([
    ['not configured', V1GpuXidQueryStatus.NOTCONFIGURED, ''],
    ['failed', V1GpuXidQueryStatus.FAILED, 'timeout'],
    ['ok without XIDs', V1GpuXidQueryStatus.OK, ''],
  ])('shows nothing about the XID query when it is %s', async (_, status, error) => {
    const topo = gpuTopologyCase('recent critical XIDs');
    topo.xidQueryStatus = status;
    topo.xidQueryError = error;
    topo.gpus.forEach((g) => {
      g.recentXids = [];
      g.health = g.pcieLinkWidth === 16 ? V1GpuHealth.OK : V1GpuHealth.LINKBELOWMAX;
    });
    setup(<GpuTopology agent={agentOf('a', topo)} />);
    await userEvent.click(within(tile('Slot 0')).getByRole('button'));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('Slot 0 on a: ok');
    expect(dialog).not.toHaveTextContent(/XID|timeout|query|Prometheus/i);
  });

  it('shows the XIDs that a failed query keeps and nothing about the failure', async () => {
    const topo = gpuTopologyCase('recent critical XIDs');
    topo.xidQueryStatus = V1GpuXidQueryStatus.FAILED;
    topo.xidQueryError = 'timeout';
    setup(<GpuTopology agent={agentOf('a', topo)} />);
    expect(
      within(tile('Slot 0')).getByRole('img', { name: 'GPU health: error' }),
    ).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(/timeout|Prometheus/i);
    await userEvent.click(within(tile('Slot 0')).getByRole('button'));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('Slot 0 on a: error');
    expect(dialog).toHaveTextContent(/Recent critical XIDs79 \(.+ to .+\)$/);
    expect(dialog).not.toHaveTextContent(/timeout|query|Prometheus|failed/i);
  });

  it('keeps the inventory when the topology is unknown', () => {
    const topo = gpuTopologyCase('NVML init failed');
    setup(<GpuTopology agent={agentOf('a', topo)} />);
    expect(
      screen.getByText('GPU topology unknown: NVML init: ERROR_LIBRARY_NOT_FOUND (12)'),
    ).toBeInTheDocument();
    expect(
      screen.getByText(`unknown; excluded: ${topo.gpus[7].uuid} (unknown)`),
    ).toBeInTheDocument();
    // 7 slot tiles and 1 excluded tile, all hollow; no grouping and no matrix.
    expect(screen.getAllByRole('group')).toHaveLength(8);
    expect(screen.getAllByRole('img', { name: 'GPU health: unknown' })).toHaveLength(8);
    expect(tile(`Excluded GPU ${topo.gpus[7].uuid}`)).toHaveClass('striped');
    // Without a bus id the button names the excluded GPU by its UUID, as the API spells it.
    expect(
      screen.getByRole('button', { name: `Details for excluded GPU ${topo.gpus[7].uuid} on a` }),
    ).toBeInTheDocument();
    expect(screen.queryByRole('region')).not.toBeInTheDocument();
    expect(screen.queryByText('Pairwise matrix')).not.toBeInTheDocument();
  });

  it('shows all GPUs excluded', () => {
    setup(<GpuTopology agent={agentOf('a', gpuTopologyCase('all 8 GPUs excluded'))} />);
    expect(screen.getByText('no slots')).toBeInTheDocument();
    expect(screen.getAllByRole('group', { name: /^Excluded GPU / })).toHaveLength(8);
  });

  it('marks pairs without usable P2P in the pairwise matrix', () => {
    setup(<GpuTopology agent={agentOf('g292', gpuTopologyCase('g292'))} />);
    expect(screen.getByText('8 PIX/NODE no-p2p(GPU_NOT_SUPPORTED)')).toBeInTheDocument();
    expect(screen.getAllByText('PCIe switch (PIX)')).toHaveLength(4);
    const table = screen.getByRole('table');
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(9);
    const cells = within(rows[1]).getAllByRole('cell');
    expect(cells[0]).toHaveTextContent('X');
    expect(cells[1]).toHaveTextContent('PIXno P2P: GNS');
    expect(cells[2]).toHaveTextContent('NODEno P2P: GNS');
  });

  it('shows a PCIe switch group for each PIX group, never one across PXB', () => {
    setup(<GpuTopology agent={agentOf('a', gpuTopologyCase('two PCIe switches'))} />);
    expect(screen.getAllByText('PCIe switch (PIX)')).toHaveLength(2);
    expect(screen.queryByText(/PXB\)/)).not.toBeInTheDocument();
    const rows = within(screen.getByRole('table')).getAllByRole('row');
    expect(within(rows[1]).getAllByRole('cell')[2]).toHaveTextContent('PXB');
  });

  it('marks unknown levels and unknown P2P with ?', () => {
    setup(<GpuTopology agent={agentOf('a', gpuTopologyCase('every pair unknown'))} />);
    const rows = within(screen.getByRole('table')).getAllByRole('row');
    const cells = within(rows[1]).getAllByRole('cell');
    expect(cells[1]).toHaveTextContent('PXBP2P ?');
    expect(cells[2]).toHaveTextContent('?P2P ?');
  });

  it('labels excluded GPUs by bus id in the matrix', () => {
    setup(<GpuTopology agent={agentOf('a', gpuTopologyCase('node01 with the exclude list'))} />);
    const headers = within(screen.getByRole('table')).getAllByRole('columnheader');
    expect(headers.map((h) => h.textContent)).toEqual([
      'GPU',
      '0',
      '1',
      '2',
      '3',
      '5',
      '6',
      '7',
      '81:00.0',
    ]);
  });

  it('has a text legend', () => {
    setup(<GpuTopologyLegend />);
    const legend = screen.getByRole('note', { name: 'GPU topology legend' });
    expect(legend).toHaveTextContent('Healthoklink below maxerrorunknown');
    expect(legend).toHaveTextContent('striped = disabled, draining or excluded');
    expect(legend).toHaveTextContent('FillFreePendingRunning');
  });
});

describe('ClusterTopology', () => {
  it('uses the GPU panel only for agents with a GPU topology', () => {
    const gpuAgent = agentOf('node02', gpuTopologyCase('node02'));
    const cpuAgent: Agent = { ...agentOf('cpu', undefined), resources: [slot(0)] };
    setup(<ClusterTopology nodes={[gpuAgent, cpuAgent]} />);
    expect(
      screen.getByRole('article', { name: 'GPU topology of agent node02' }),
    ).toBeInTheDocument();
    expect(screen.queryByRole('article', { name: 'GPU topology of agent cpu' })).toBeNull();
    expect(screen.getByText('cpu')).toBeInTheDocument();
    expect(screen.getAllByRole('note', { name: 'GPU topology legend' })).toHaveLength(1);
  });

  it('shows no legend without GPU topology', () => {
    const cpuAgent: Agent = { ...agentOf('cpu', undefined), resources: [slot(0)] };
    setup(<ClusterTopology nodes={[cpuAgent]} />);
    expect(screen.queryByRole('note', { name: 'GPU topology legend' })).toBeNull();
  });
});
