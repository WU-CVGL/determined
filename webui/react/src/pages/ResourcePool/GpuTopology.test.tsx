import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { ThemeProvider } from 'components/ThemeProvider';
import { gpuTopologyCase } from 'fixtures/gpuTopologyCases';
import { V1GpuTopology } from 'services/api-ts-sdk';
import { Agent, Resource, ResourceState, ResourceType } from 'types';
import { GPU_EXCLUDED_TEXT, GPU_NARROW_LINK_TEXT } from 'utils/gpuTopology';

import ClusterTopology from './ClusterTopology';
import GpuTopology, { GPU_TOPOLOGY_DOCS_PATH, GpuTopologyLegend } from './GpuTopology';

vi.mock('hew/Tooltip');

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
): Agent => ({
  gpuTopology,
  id,
  registeredTime: 0,
  resourcePools: ['pool'],
  resources: (gpuTopology?.gpus ?? [])
    .filter((g) => !g.excluded)
    .map((g) => slot(g.deviceId, overrides[g.deviceId])),
  slotStats: { brandStats: {}, typeStats: {} },
});

const setup = (children: React.ReactNode) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>{children}</ThemeProvider>
    </UIProvider>,
  );

const tile = (name: string) => screen.getByRole('group', { name: new RegExp(`^${name},`) });

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
    expect(
      screen.getByText('narrow: 3,5 (x8 of x16 at start); excluded: 81:00.0'),
    ).toBeInTheDocument();
    // The fill states, then the slots that take no new work, then the excluded GPUs.
    expect(
      screen.getByText('7 slots: 1 running, 1 pending, 5 free, 1 disabled, 1 draining; 1 excluded'),
    ).toBeInTheDocument();

    // One dot per GPU, labelled for screen readers: amber on the x8 slots, green elsewhere.
    expect(
      screen.getAllByRole('img', { name: 'GPU health: link below max at start' }),
    ).toHaveLength(2);
    expect(screen.getAllByRole('img', { name: 'GPU health: ok' })).toHaveLength(6);
    expect(within(tile('Slot 3')).getByRole('img').getAttribute('aria-label')).toBe(
      'GPU health: link below max at start',
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

  it('shows the details on hover and pins them on click', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    const button = within(tile('Slot 3')).getByRole('button');

    await userEvent.hover(button);
    const tooltip = await screen.findByRole('tooltip');
    expect(tooltip).toHaveTextContent('Slot 3 on node01: link below max at start');
    expect(tooltip).toHaveTextContent('UUID');
    expect(tooltip).toHaveTextContent('0000:61:00.0');
    expect(tooltip).toHaveTextContent(
      'Link at agent startx8 of x16, Gen4 of Gen4 (an observation, not a confirmed fault)',
    );
    expect(tooltip).toHaveTextContent('NVML errors at agent startnone');
    expect(tooltip).toHaveTextContent('Recent critical XIDsnot collected');
    expect(tooltip).toHaveTextContent('(agent clock, at agent start)');
    expect(tooltip).toHaveTextContent(GPU_NARROW_LINK_TEXT);
    await userEvent.unhover(button);

    await userEvent.click(button);
    const dialog = await screen.findByRole('dialog', { name: 'Details for slot 3 on node01' });
    expect(button).toHaveAttribute('aria-expanded', 'true');
    // A pointer pin leaves focus where the click put it.
    expect(button).toHaveFocus();
    expect(dialog).toHaveTextContent(GPU_NARROW_LINK_TEXT);
    const docs = within(dialog).getByRole('link', { name: 'GPU topology and health' });
    expect(docs.getAttribute('href')).toContain(GPU_TOPOLOGY_DOCS_PATH);

    await userEvent.click(button);
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
  });

  it('counts the disabled and draining slots of an agent', () => {
    const agent = agentOf('node02', gpuTopologyCase('node02'), {
      0: { container: { id: 'c0', state: ResourceState.Running }, enabled: false },
      5: { enabled: false },
    });
    setup(<GpuTopology agent={agent} />);
    expect(
      screen.getByText('8 slots: 1 running, 0 pending, 7 free, 2 disabled'),
    ).toBeInTheDocument();
  });

  it('shows the details on focus, hides the tooltip while pinned, and unpins on Escape', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    const button = within(tile('Slot 3')).getByRole('button');

    act(() => button.focus());
    const tooltip = await screen.findByRole('tooltip');
    expect(tooltip).toHaveTextContent('Slot 3 on node01: link below max at start');
    expect(tooltip).toHaveTextContent(GPU_NARROW_LINK_TEXT);

    // Pinned, the popover replaces the tooltip, although the pointer rests on the button. (antd
    // gives the popover the role tooltip too, and jsdom does not apply antd's hidden class.)
    await userEvent.click(button);
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    await waitFor(() => expect(tooltip.closest('.ant-tooltip')).toHaveClass('ant-tooltip-hidden'));

    await userEvent.keyboard('{Escape}');
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(button).toHaveFocus();
  });

  it('moves focus into the details pinned from the keyboard and back on Escape', async () => {
    const agent = agentOf('node01', gpuTopologyCase('node01 with the exclude list'));
    setup(<GpuTopology agent={agent} />);
    const button = within(tile('Slot 3')).getByRole('button');
    act(() => button.focus());

    // The popover is portalled to the end of the page: Tab must reach its close button and its docs
    // link next.
    await userEvent.keyboard('{Enter}');
    const dialog = await screen.findByRole('dialog', { name: 'Details for slot 3 on node01' });
    expect(button).toHaveAttribute('aria-expanded', 'true');
    await waitFor(() => expect(dialog).toHaveFocus());
    await userEvent.tab();
    expect(within(dialog).getByRole('button', { name: 'Close details' })).toHaveFocus();
    await userEvent.tab();
    expect(within(dialog).getByRole('link', { name: 'GPU topology and health' })).toHaveFocus();

    await userEvent.keyboard('{Escape}');
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(button).toHaveFocus();
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
    expect(legend).toHaveTextContent('Healthoklink below max at starterrorunknown');
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
