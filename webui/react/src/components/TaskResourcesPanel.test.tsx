import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import TaskResourcesPanel from './TaskResourcesPanel';

vi.mock('hooks/useTaskResourcesEnabled', () => ({
  default: () => true,
}));
vi.mock('components/TaskResourceChart', () => ({
  default: ({ series }: { series: { labels: { allocation_id: string } }[] }) => (
    <div>{series.map((item) => item.labels.allocation_id).join(',')}</div>
  ),
}));

const HOUR = 3600;
const DAY = 86400;
const CLAMP_CAPTION = /longer than the 7-day query limit/;

interface Reply {
  ok: boolean;
  status?: number;
  json?: () => Promise<unknown>;
}

const response = (allocation: string): Reply => ({
  json: () =>
    Promise.resolve({
      enabled: true,
      series: [{ labels: { allocation_id: allocation }, metric: 'cpu_cores', samples: [[100, 1]] }],
      warnings: [],
    }),
  ok: true,
});
const iso = (seconds: number) => new Date(seconds * 1000).toISOString();
const allocationsReply = (
  items: { allocation_id: string; container_start: number | null; end: number | null }[],
): Reply => ({
  json: () =>
    Promise.resolve({
      allocations: items.map((item) => ({
        ...item,
        container_start: item.container_start === null ? null : iso(item.container_start),
        end: item.end === null ? null : iso(item.end),
      })),
    }),
  ok: true,
});
const nowSeconds = () => Math.floor(Date.now() / 1000);

const isAllocationsUrl = (url: unknown) => String(url).includes('/allocations');
// Routes the allocation list and the resource series requests to separate replies.
const routedFetch = (
  allocations: () => Promise<Reply>,
  series: () => Promise<Reply> = () => Promise.resolve(response('allocation')),
) => vi.fn((url: string) => (isAllocationsUrl(url) ? allocations() : series()));
interface FetchCalls {
  mock: { calls: unknown[][] };
}
const seriesCalls = (fetchMock: FetchCalls) =>
  fetchMock.mock.calls.filter(([url]) => !isAllocationsUrl(url));
const seriesRange = (fetchMock: FetchCalls, index: number) => {
  const params = new URL(String(seriesCalls(fetchMock)[index][0]), 'http://master').searchParams;
  return {
    end: Number(params.get('end')),
    start: Number(params.get('start')),
    step: Number(params.get('step')),
  };
};
const firstSeriesRange = async (fetchMock: FetchCalls) => {
  await waitFor(() => expect(seriesCalls(fetchMock).length).toBeGreaterThan(0));
  return seriesRange(fetchMock, 0);
};

const page = (
  taskId: string,
  initialAllocationId?: string,
  startTime = iso(nowSeconds() - 3 * HOUR),
) => (
  <UIProvider theme={DefaultTheme.Light}>
    <TaskResourcesPanel
      initialAllocationId={initialAllocationId}
      startTime={startTime}
      taskId={taskId}
    />
  </UIProvider>
);

afterEach(() => vi.unstubAllGlobals());

it('retains loaded samples when an allocation deep link changes within the same task', async () => {
  vi.stubGlobal(
    'fetch',
    routedFetch(
      () => Promise.resolve(allocationsReply([])),
      () => Promise.resolve(response('selected-allocation')),
    ),
  );
  const startTime = iso(nowSeconds() - HOUR);
  const view = render(page('task', undefined, startTime));
  expect(await screen.findByText('selected-allocation')).toBeInTheDocument();
  view.rerender(page('task', 'selected-allocation', startTime));
  expect(screen.getAllByText('selected-allocation').length).toBeGreaterThan(1);
});

it('ignores a late response after changing tasks and aborts the previous request', async () => {
  let resolveOld!: (value: Reply) => void;
  const fetchMock = vi.fn((url: string) => {
    if (isAllocationsUrl(url)) return Promise.resolve(allocationsReply([]));
    if (url.includes('/old-task?'))
      return new Promise<Reply>((resolve) => {
        resolveOld = resolve;
      });
    return Promise.resolve(response('current-allocation'));
  });
  vi.stubGlobal('fetch', fetchMock);
  const startTime = iso(nowSeconds() - HOUR);
  const view = render(page('old-task', undefined, startTime));
  await waitFor(() => expect(seriesCalls(fetchMock)).toHaveLength(1));
  view.rerender(page('new-task', undefined, startTime));
  expect((seriesCalls(fetchMock)[0][1] as RequestInit).signal?.aborted).toBe(true);
  expect(await screen.findByText('current-allocation')).toBeInTheDocument();
  await act(async () => {
    resolveOld(response('old-allocation'));
    await Promise.resolve();
  });
  expect(screen.queryByText('old-allocation')).not.toBeInTheDocument();
});

it('labels retained charts when a refresh fails instead of presenting them as current data', async () => {
  const series = vi
    .fn()
    .mockResolvedValueOnce(response('retained-allocation'))
    .mockResolvedValueOnce({ ok: false, status: 502 });
  vi.stubGlobal(
    'fetch',
    routedFetch(() => Promise.resolve(allocationsReply([])), series),
  );
  render(page('task', undefined, iso(nowSeconds() - HOUR)));
  expect(await screen.findByText('retained-allocation')).toBeInTheDocument();
  await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  expect(
    await screen.findByText('The charts below retain the last successful response.'),
  ).toBeInTheDocument();
  expect(screen.getByText('retained-allocation')).toBeInTheDocument();
});

it('defaults to Since start from the earliest container start of all allocations', async () => {
  const now = nowSeconds();
  const fetchMock = routedFetch(() =>
    Promise.resolve(
      allocationsReply([
        { allocation_id: 'task.2', container_start: now - 30 * 60, end: null },
        { allocation_id: 'task.1', container_start: now - 2 * HOUR, end: now - HOUR },
        { allocation_id: 'task.3', container_start: null, end: null },
      ]),
    ),
  );
  vi.stubGlobal('fetch', fetchMock);
  render(page('task'));
  expect(screen.getByText('Since start')).toBeInTheDocument();
  const range = await firstSeriesRange(fetchMock);
  // The task was submitted three hours ago, but resources were first held two hours ago.
  expect(range.start).toBe(now - 2 * HOUR);
  expect(range.end).toBeGreaterThanOrEqual(now);
  expect(range.end).toBeLessThanOrEqual(nowSeconds());
  expect(range.step).toBe(15);
  // The series wait for the allocation list, so they are requested once.
  expect(await screen.findByText('allocation')).toBeInTheDocument();
  expect(seriesCalls(fetchMock)).toHaveLength(1);
  expect(screen.queryByText(CLAMP_CAPTION)).not.toBeInTheDocument();
  expect(screen.queryByText(/No container start is recorded/)).not.toBeInTheDocument();
});

it('covers one allocation from its container start to its end when it is selected', async () => {
  const now = nowSeconds();
  const fetchMock = routedFetch(() =>
    Promise.resolve(
      allocationsReply([
        { allocation_id: 'task.1', container_start: now - 2 * HOUR, end: now - HOUR },
        { allocation_id: 'task.2', container_start: now - 30 * 60, end: null },
      ]),
    ),
  );
  vi.stubGlobal('fetch', fetchMock);
  const view = render(page('task', 'task.1'));
  expect(await firstSeriesRange(fetchMock)).toEqual({
    end: now - HOUR,
    start: now - 2 * HOUR,
    step: 15,
  });

  // A running allocation ends now.
  view.rerender(page('task', 'task.2'));
  await waitFor(() => expect(seriesCalls(fetchMock)).toHaveLength(2));
  const range = seriesRange(fetchMock, 1);
  expect(range.start).toBe(now - 30 * 60);
  expect(range.end).toBeGreaterThanOrEqual(now);
});

it('shows the most recent 7 days when the time since start is longer, and says so', async () => {
  const now = nowSeconds();
  const fetchMock = routedFetch(() =>
    Promise.resolve(
      allocationsReply([{ allocation_id: 'task.1', container_start: now - 10 * DAY, end: null }]),
    ),
  );
  vi.stubGlobal('fetch', fetchMock);
  render(page('task', undefined, iso(now - 11 * DAY)));
  const range = await firstSeriesRange(fetchMock);
  expect(range.end - range.start).toBe(7 * DAY);
  expect(range.step).toBe(421);
  expect(Math.floor((range.end - range.start) / range.step) + 1).toBeLessThanOrEqual(1440);
  expect(await screen.findByText(CLAMP_CAPTION)).toBeInTheDocument();
});

it('falls back to the task start without an error when the allocation list is unavailable', async () => {
  const now = nowSeconds();
  const failures: [string, () => Promise<Reply>][] = [
    ['missing', () => Promise.resolve({ ok: false, status: 404 })],
    ['html', () => Promise.resolve({ json: () => Promise.resolve('<html>'), ok: true })],
    ['offline', () => Promise.reject(new Error('network down'))],
  ];
  for (const [taskId, reply] of failures) {
    const fetchMock = routedFetch(reply);
    vi.stubGlobal('fetch', fetchMock);
    const view = render(page(taskId, undefined, iso(now - 3 * HOUR)));
    expect((await firstSeriesRange(fetchMock)).start).toBe(now - 3 * HOUR);
    expect(await screen.findByText('allocation')).toBeInTheDocument();
    expect(screen.queryByText(/could not be loaded/)).not.toBeInTheDocument();
    expect(screen.queryByText(/No container start is recorded/)).not.toBeInTheDocument();
    view.unmount();
  }
});

it('begins at the task start and says so while no allocation has a container start', async () => {
  const now = nowSeconds();
  const fetchMock = routedFetch(() =>
    Promise.resolve(
      allocationsReply([{ allocation_id: 'task.1', container_start: null, end: null }]),
    ),
  );
  vi.stubGlobal('fetch', fetchMock);
  render(page('task', undefined, iso(now - HOUR)));
  expect((await firstSeriesRange(fetchMock)).start).toBe(now - HOUR);
  expect(await screen.findByText(/No container start is recorded yet,/)).toBeInTheDocument();
});

it('begins at the task start of an ended task whose allocations never got resources', async () => {
  const now = nowSeconds();
  const fetchMock = routedFetch(() =>
    Promise.resolve(
      allocationsReply([{ allocation_id: 'task.1', container_start: null, end: now - HOUR }]),
    ),
  );
  vi.stubGlobal('fetch', fetchMock);
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <TaskResourcesPanel endTime={iso(now - HOUR)} startTime={iso(now - 2 * HOUR)} taskId="task" />
    </UIProvider>,
  );
  expect(await firstSeriesRange(fetchMock)).toEqual({
    end: now - HOUR,
    start: now - 2 * HOUR,
    step: 15,
  });
  expect(await screen.findByText(/No container start is recorded,/)).toBeInTheDocument();
});

it('keeps the other ranges relative to now and the task start', async () => {
  const user = userEvent.setup();
  const now = nowSeconds();
  const fetchMock = routedFetch(() =>
    Promise.resolve(
      allocationsReply([{ allocation_id: 'task.1', container_start: now - 2 * HOUR, end: null }]),
    ),
  );
  vi.stubGlobal('fetch', fetchMock);
  render(page('task', undefined, iso(now - 3 * HOUR)));
  await firstSeriesRange(fetchMock);

  await user.click(screen.getAllByRole('combobox')[0]);
  await user.click((await screen.findAllByText('Last hour')).at(-1) as HTMLElement);
  await waitFor(() => expect(seriesCalls(fetchMock)).toHaveLength(2));
  const hour = seriesRange(fetchMock, 1);
  expect(hour.end - hour.start).toBe(HOUR);

  // Last 7 days stops at the task start, as before.
  await user.click(screen.getAllByRole('combobox')[0]);
  await user.click((await screen.findAllByText('Last 7 days')).at(-1) as HTMLElement);
  await waitFor(() => expect(seriesCalls(fetchMock)).toHaveLength(3));
  expect(seriesRange(fetchMock, 2).start).toBe(now - 3 * HOUR);
  expect(screen.queryByText(CLAMP_CAPTION)).not.toBeInTheDocument();
});
