import { fireEvent, render, waitFor } from '@testing-library/react';
import { SyncProvider } from 'hew/LineChart/SyncProvider';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { ThemeProvider } from 'components/ThemeProvider';
import { RESOURCE_METRICS, ResourceRange, ResourceSeries } from 'utils/taskResources';

import TaskResourceChart from './TaskResourceChart';

// jsdom has no 2D canvas; uPlot only needs calls that do nothing.
const canvasContext = (): CanvasRenderingContext2D =>
  new Proxy({} as CanvasRenderingContext2D, {
    get: (_target, key) => (key === 'measureText' ? () => ({ width: 0 }) : () => undefined),
    set: () => true,
  });

beforeEach(() => {
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(
    canvasContext as unknown as typeof HTMLCanvasElement.prototype.getContext,
  );
});
afterEach(() => vi.restoreAllMocks());

const UUID = 'GPU-1a2b3c4d-0000-1111-2222-333344445555';
const gpu = (index: number, uuid: string): ResourceSeries => ({
  labels: {
    allocation_id: 'task.1',
    gpu_index: index,
    gpu_uuid: uuid,
    host_gpu_index: String(index + 4),
    model_name: 'NVIDIA GeForce RTX 4090',
    node: 'cvgl-node02.lan',
    pci_bus_id: `00000000:${41 + index}:00.0`,
  },
  metric: 'gpu_utilization_percent',
  samples: [
    [100, 10 * (index + 1)],
    [115, 20],
  ],
});

const metric = RESOURCE_METRICS.find((item) => item.key === 'gpu_utilization_percent');
const range = { end: 115, start: 100, step: 15 };
const chart = (series: ResourceSeries[], chartRange: ResourceRange = range) => {
  if (!metric) throw new Error('missing metric');
  return (
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <SyncProvider>
          <TaskResourceChart metric={metric} range={chartRange} series={series} />
        </SyncProvider>
      </ThemeProvider>
    </UIProvider>
  );
};
const legendLabels = (container: HTMLElement) =>
  Array.from(container.querySelectorAll('.u-legend .u-label')).map((label) => label.textContent);

it('renders the live legend with GPU numbers and details on hover', async () => {
  const view = render(chart([gpu(0, UUID), gpu(1, 'GPU-9f8e7d6c-0000-1111-2222-333344445555')]));
  await waitFor(() =>
    expect(view.container.querySelectorAll('.u-legend .u-series')).toHaveLength(3),
  );
  const rows = Array.from(view.container.querySelectorAll<HTMLElement>('.u-legend .u-series'));
  expect(rows.map((row) => row.querySelector('.u-label')?.textContent)).toEqual([
    'Time',
    'GPU 0',
    'GPU 1',
  ]);
  // Every entry keeps its live value cell.
  rows.forEach((row) => expect(row.querySelector('.u-value')).not.toBeNull());
  expect(rows[0].title).toBe('');
  expect(rows[1].title.split('\n')).toEqual([
    `GPU UUID: ${UUID}`,
    'Host: cvgl-node02.lan',
    'Allocation: task.1',
    'PCI bus ID: 00000000:41:00.0',
    'Host GPU index: 4',
    'Model: NVIDIA GeForce RTX 4090',
  ]);
  expect(rows[2].title).toContain('PCI bus ID: 00000000:42:00.0');
});

it('keeps the chart and its hidden series when it re-renders with the same labels', async () => {
  const series = [gpu(0, UUID), gpu(1, 'GPU-9f8e7d6c-0000-1111-2222-333344445555')];
  const view = render(chart(series));
  await waitFor(() => expect(legendLabels(view.container)).toEqual(['Time', 'GPU 0', 'GPU 1']));
  const legend = view.container.querySelector('.u-legend');
  const rows = () => view.container.querySelectorAll<HTMLElement>('.u-legend .u-series');
  // Hide GPU 0 from the legend.
  const label = rows()[1].querySelector('.u-label');
  if (!label) throw new Error('missing legend label');
  fireEvent.click(label);
  expect(rows()[1]).toHaveClass('u-off');

  // A refresh of an ended task hands over new arrays and a new range object with the same labels
  // and bounds: the chart, its legend and the hidden series stay.
  view.rerender(
    chart(
      series.map((item) => ({ ...item, samples: [...item.samples] })),
      { ...range },
    ),
  );
  expect(view.container.querySelector('.u-legend')).toBe(legend);
  expect(rows()[1]).toHaveClass('u-off');
  expect(rows()[1].title).toContain(UUID);

  // New labels rebuild the legend.
  view.rerender(
    chart([{ ...series[0], labels: { ...series[0].labels, gpu_index: undefined } }, series[1]]),
  );
  await waitFor(() =>
    expect(legendLabels(view.container)).toEqual(['Time', 'GPU 1a2b3c4d', 'GPU 1']),
  );
});
