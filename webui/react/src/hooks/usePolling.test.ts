import { act, renderHook } from '@testing-library/react';

import usePolling from './usePolling';

vi.mock('components/ThemeProvider', () => ({ default: () => ({ ui: { isPageHidden: false } }) }));

const INTERVAL = 1000;

/** A promise that the test resolves by hand. */
function deferred() {
  let resolve: () => void = () => undefined;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

/** A polling function whose first request stays pending until the test resolves it. */
const pendingFirstRequest = () => {
  const first = deferred();
  const pollingFn = vi.fn(() => Promise.resolve());
  pollingFn.mockReturnValueOnce(first.promise);
  return { first, pollingFn };
};

const settle = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));

describe('usePolling', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('polls again an interval after the first request completes', async () => {
    const { first, pollingFn } = pendingFirstRequest();
    renderHook(() => usePolling(pollingFn, { interval: INTERVAL }));
    expect(pollingFn).toHaveBeenCalledTimes(1);

    first.resolve();
    await settle();
    await settle(INTERVAL);
    expect(pollingFn).toHaveBeenCalledTimes(2);
    await settle(INTERVAL);
    expect(pollingFn).toHaveBeenCalledTimes(3);
  });

  it('polls no more after a stop during the first request', async () => {
    const { first, pollingFn } = pendingFirstRequest();
    const { result } = renderHook(() => usePolling(pollingFn, { interval: INTERVAL }));
    expect(pollingFn).toHaveBeenCalledTimes(1);

    act(() => result.current.stopPolling());
    first.resolve();
    await settle();
    await settle(5 * INTERVAL);
    expect(pollingFn).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('polls no more after an unmount during the first request', async () => {
    const { first, pollingFn } = pendingFirstRequest();
    const { unmount } = renderHook(() => usePolling(pollingFn, { interval: INTERVAL }));

    unmount();
    first.resolve();
    await settle();
    await settle(5 * INTERVAL);
    expect(pollingFn).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('polls one last time after a graceful stop during the first request', async () => {
    const { first, pollingFn } = pendingFirstRequest();
    const { result } = renderHook(() => usePolling(pollingFn, { interval: INTERVAL }));

    act(() => result.current.stopPolling({ terminateGracefully: true }));
    first.resolve();
    await settle();
    await settle(INTERVAL);
    expect(pollingFn).toHaveBeenCalledTimes(2);
    await settle(5 * INTERVAL);
    expect(pollingFn).toHaveBeenCalledTimes(2);
  });

  it('polls once an interval after a stop and a restart during the first request', async () => {
    const { first, pollingFn } = pendingFirstRequest();
    const { result } = renderHook(() => usePolling(pollingFn, { interval: INTERVAL }));

    act(() => result.current.stopPolling());
    act(() => {
      result.current.startPolling();
    });
    // The new start's own first request.
    expect(pollingFn).toHaveBeenCalledTimes(2);
    first.resolve();
    await settle();
    await settle(INTERVAL);
    expect(pollingFn).toHaveBeenCalledTimes(3);
    await settle(INTERVAL);
    expect(pollingFn).toHaveBeenCalledTimes(4);
  });
});
