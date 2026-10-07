import { PoolAccessResult } from 'utils/resourcePoolAccess';

import poolAccessChange from './poolAccessChange';

const done = (poolName: string): PoolAccessResult => ({
  confirmedRequests: 1,
  confirmedUsernames: 0,
  ok: true,
  poolName,
  requests: 1,
  totalUsernames: 0,
  warnings: [],
});

/** A promise that the test settles. */
function deferred<T>() {
  let resolve: (value: T) => void = () => undefined;
  let reject: (reason: unknown) => void = () => undefined;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, reject, resolve };
}

describe('poolAccessChange', () => {
  beforeEach(() => poolAccessChange.dismiss());

  it('refuses a change while another one runs, before sending anything', async () => {
    const first = deferred<PoolAccessResult[]>();
    const firstSend = vi.fn(() => first.promise);
    const secondSend = vi.fn(() => Promise.resolve([done('b')]));

    const running = poolAccessChange.run('grant', firstSend);
    // The guard holds from the call on, before the first change awaits anything.
    expect(poolAccessChange.isRunning.get()).toBe(true);
    expect(await poolAccessChange.run('revoke', secondSend)).toBeUndefined();
    expect(secondSend).not.toHaveBeenCalled();
    // A running change cannot be dismissed.
    poolAccessChange.dismiss();
    expect(poolAccessChange.change.get()).toEqual({ action: 'grant' });

    first.resolve([done('a')]);
    expect(await running).toEqual([done('a')]);
    expect(poolAccessChange.isRunning.get()).toBe(false);
    expect(poolAccessChange.change.get()).toEqual({ action: 'grant', results: [done('a')] });

    expect(await poolAccessChange.run('revoke', secondSend)).toEqual([done('b')]);
    expect(poolAccessChange.change.get()).toEqual({ action: 'revoke', results: [done('b')] });
    poolAccessChange.dismiss();
    expect(poolAccessChange.change.get()).toBeUndefined();
  });

  it('does not stay running when a change throws', async () => {
    const failing = deferred<PoolAccessResult[]>();
    const running = poolAccessChange.run('restrict', () => failing.promise);
    failing.reject(new Error('unexpected'));
    await expect(running).rejects.toThrow('unexpected');
    expect(poolAccessChange.change.get()).toBeUndefined();
    expect(await poolAccessChange.run('public', () => Promise.resolve([done('a')]))).toEqual([
      done('a'),
    ]);
  });

  it('counts the shown tabs', () => {
    expect(poolAccessChange.isTabShown).toBe(false);
    const hideFirst = poolAccessChange.showTab();
    const hideSecond = poolAccessChange.showTab();
    hideFirst();
    expect(poolAccessChange.isTabShown).toBe(true);
    hideSecond();
    expect(poolAccessChange.isTabShown).toBe(false);
  });
});
