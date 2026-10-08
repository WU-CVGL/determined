import { resourcePoolAccessResponse } from 'fixtures/resourcePoolAccess';
import { mapResourcePoolAccess } from 'services/decoder';
import { DetailedUser } from 'types';
import { DetError } from 'utils/error';

import {
  changeUsersInPools,
  chunkUsernames,
  matchesPoolSearch,
  parsePastedUsernames,
  poolAccessErrorMessage,
  resolveUsernames,
  RESOURCE_POOL_ACCESS_BODY_BUDGET,
  RESOURCE_POOL_ACCESS_MAX_BODY_BYTES,
  setModeInPools,
  usernamesBodyBytes,
} from './resourcePoolAccess';

const pools = resourcePoolAccessResponse.resource_pools.map(mapResourcePoolAccess);
const pool = (name: string) => {
  const found = pools.find((p) => p.poolName === name);
  if (!found) throw new Error(name);
  return found;
};

const users: DetailedUser[] = [
  { id: 1, isActive: true, isAdmin: false, username: 'alice' },
  { id: 2, isActive: true, isAdmin: false, username: 'bob' },
  { id: 3, isActive: false, isAdmin: false, username: 'carol' },
  { id: 4, isActive: true, isAdmin: true, username: 'dave' },
  { id: 5, isActive: true, isAdmin: false, username: 'john doe' },
];

const accepted = ({ poolName }: { poolName: string }) =>
  Promise.resolve(mapResourcePoolAccess({ pool_name: poolName, warnings: ['w'] }));

describe('resourcePoolAccess', () => {
  describe('the decoded API response', () => {
    it('keeps every field of an item', () => {
      const a100Warnings = [
        '"gpu-a100" is the cluster\'s default compute pool: submissions that omit ' +
          'resources.resource_pool are refused for users without a grant on "gpu-a100"',
        '"gpu-a100" is the default compute pool of workspace "vision": submissions there that ' +
          'omit resources.resource_pool are refused for users without a grant on "gpu-a100"',
      ];
      expect(pool('gpu-a100')).toEqual({
        defaultAux: false,
        defaultCompute: true,
        exists: true,
        mode: 'restricted',
        poolName: 'gpu-a100',
        restrictedAt: '2026-10-01T08:00:00Z',
        restrictedBy: 'admin',
        users: [
          { active: true, admin: false, id: 7, username: 'alice' },
          { active: false, admin: false, id: 9, username: 'carol' },
        ],
        warnings: a100Warnings,
        warningsIfRestricted: a100Warnings,
        workspaceDefaults: [{ kind: 'compute', workspace: 'vision', workspaceId: 4 }],
      });
      expect(pool('cpu').restrictedBy).toBeUndefined();
      expect(pool('cpu').warnings).toEqual([]);
      expect(pool('cpu').warningsIfRestricted).toHaveLength(1);
    });

    it('reads missing lists as empty', () => {
      const decoded = mapResourcePoolAccess({ pool_name: 'gpu', warnings: null });
      expect(decoded.warnings).toEqual([]);
      expect(decoded.warningsIfRestricted).toEqual([]);
    });
  });

  it('matches pools by name, granted user, and workspace', () => {
    expect(matchesPoolSearch(pool('gpu-a100'), '')).toBe(true);
    expect(matchesPoolSearch(pool('gpu-a100'), 'A100')).toBe(true);
    expect(matchesPoolSearch(pool('gpu-a100'), 'carol')).toBe(true);
    expect(matchesPoolSearch(pool('gpu-a100'), 'vis')).toBe(true);
    expect(matchesPoolSearch(pool('gpu-a100'), 'bob')).toBe(false);
    // Search text is not a regular expression.
    expect(matchesPoolSearch(pool('gpu-a100'), '(')).toBe(false);
  });

  describe('parsePastedUsernames', () => {
    it('splits at new lines, commas, semicolons, and white space, without duplicates', () => {
      expect(parsePastedUsernames(' alice, bob;carol\n\n bob\tdave \r\n', new Set())).toEqual([
        'alice',
        'bob',
        'carol',
        'dave',
      ]);
    });

    it('keeps a line that is a known username whole', () => {
      expect(parsePastedUsernames('john doe\njane roe', new Set(['john doe']))).toEqual([
        'john doe',
        'jane',
        'roe',
      ]);
    });
  });

  describe('resolveUsernames', () => {
    it('unites users, group members, and pasted names, without duplicates', () => {
      const resolved = resolveUsernames(
        users,
        [1, 3],
        [[{ username: 'bob' }, { username: 'alice' }], [{ username: 'bob' }]],
        'alice\njohn doe\nzed, yann',
      );
      expect(resolved.usernames).toEqual(['alice', 'bob', 'carol', 'john doe']);
      expect(resolved.unknown).toEqual(['zed', 'yann']);
      expect(resolved.inactive).toEqual(['carol']);
      expect(resolved.admins).toEqual([]);
      expect(resolved.counts).toEqual({ fromGroups: 3, fromPaste: 4, fromUsers: 2 });
    });

    it('flags administrators and knows group members missing from the user list', () => {
      const resolved = resolveUsernames(
        users,
        [4],
        [[{ active: false, admin: false, username: 'new-member' }]],
        'new-member',
      );
      expect(resolved.usernames).toEqual(['dave', 'new-member']);
      expect(resolved.admins).toEqual(['dave']);
      expect(resolved.inactive).toEqual(['new-member']);
      expect(resolved.unknown).toEqual([]);
    });
  });

  describe('chunkUsernames', () => {
    it('keeps every request body within the budget, in order', () => {
      const names = Array.from({ length: 50 }, (_, i) => `user-${i}`);
      const chunks = chunkUsernames(names, 120);
      expect(chunks.length).toBeGreaterThan(1);
      chunks.forEach((chunk) => expect(usernamesBodyBytes(chunk)).toBeLessThanOrEqual(120));
      expect(chunks.flat()).toEqual(names);
      // Each chunk is as full as the budget allows.
      chunks.slice(0, -1).forEach((chunk, i) => {
        expect(usernamesBodyBytes([...chunk, chunks[i + 1][0]])).toBeGreaterThan(120);
      });
    });

    it('counts bytes, not characters', () => {
      const names = ['ééééé', 'üüüüü', 'aaaaa'];
      // {"usernames":["ééééé"]} is 28 bytes for 23 characters.
      expect(usernamesBodyBytes(['ééééé'])).toBe(28);
      expect(chunkUsernames(names, 30)).toEqual([['ééééé'], ['üüüüü'], ['aaaaa']]);
      expect(chunkUsernames(names, 40)).toEqual([['ééééé'], ['üüüüü', 'aaaaa']]);
    });

    it('sends a username too long for any request alone', () => {
      expect(chunkUsernames(['a', 'x'.repeat(100), 'b'], 40)).toEqual([
        ['a'],
        ['x'.repeat(100)],
        ['b'],
      ]);
    });

    it('stays below the master limit by default', () => {
      expect(RESOURCE_POOL_ACCESS_BODY_BUDGET).toBeLessThan(RESOURCE_POOL_ACCESS_MAX_BODY_BYTES);
      const names = Array.from({ length: 5000 }, (_, i) => `a-rather-long-username-${i}`);
      const chunks = chunkUsernames(names);
      expect(chunks.length).toBeGreaterThan(1);
      chunks.forEach((chunk) =>
        expect(usernamesBodyBytes(chunk)).toBeLessThanOrEqual(RESOURCE_POOL_ACCESS_BODY_BUDGET),
      );
      expect(chunkUsernames([])).toEqual([]);
    });
  });

  describe('changeUsersInPools', () => {
    it('sends each chunk to each pool and keeps the last warnings', async () => {
      const request = vi.fn(accepted);
      const results = await changeUsersInPools(['p1', 'p2'], ['a', 'b', 'c'], request, {
        budget: 24,
      });
      expect(request.mock.calls.map(([params]) => params)).toEqual([
        { poolName: 'p1', usernames: ['a', 'b'] },
        { poolName: 'p1', usernames: ['c'] },
        { poolName: 'p2', usernames: ['a', 'b'] },
        { poolName: 'p2', usernames: ['c'] },
      ]);
      expect(results).toEqual([
        {
          confirmedRequests: 2,
          confirmedUsernames: 3,
          ok: true,
          poolName: 'p1',
          requests: 2,
          totalUsernames: 3,
          warnings: ['w'],
        },
        {
          confirmedRequests: 2,
          confirmedUsernames: 3,
          ok: true,
          poolName: 'p2',
          requests: 2,
          totalUsernames: 3,
          warnings: ['w'],
        },
      ]);
    });

    it('ends a pool at its first failure, without retrying, and goes on', async () => {
      const request = vi.fn(({ poolName, usernames }: { poolName: string; usernames: string[] }) =>
        poolName === 'p1' && usernames[0] === 'c'
          ? Promise.reject(
              new DetError(new Response('', { status: 413 }), {
                publicMessage: 'request body exceeds 64 KiB',
              }),
            )
          : accepted({ poolName }),
      );
      const results = await changeUsersInPools(['p1', 'p2'], ['a', 'b', 'c', 'd'], request, {
        budget: 24,
      });
      expect(request).toHaveBeenCalledTimes(4);
      expect(results[0]).toMatchObject({
        confirmedRequests: 1,
        confirmedUsernames: 2,
        error: '413 request body exceeds 64 KiB',
        ok: false,
        requests: 2,
      });
      expect(results[1]).toMatchObject({ confirmedRequests: 2, ok: true });
    });

    it('fails a request that does not answer in time, aborts it, and goes on', async () => {
      const signals: AbortSignal[] = [];
      const request = vi.fn(
        ({ poolName }: { poolName: string }, options?: { signal?: AbortSignal }) => {
          if (options?.signal) signals.push(options.signal);
          // The request ignores its signal: the change ends anyway.
          return poolName === 'p1' ? new Promise<never>(() => undefined) : accepted({ poolName });
        },
      );
      const results = await changeUsersInPools(['p1', 'p2'], ['a', 'b', 'c'], request, {
        budget: 24,
        timeoutMs: 20,
      });
      expect(request).toHaveBeenCalledTimes(3);
      expect(signals.map((signal) => signal.aborted)).toEqual([true, false, false]);
      expect(results[0]).toMatchObject({
        confirmedRequests: 0,
        error: 'no answer within 0.02 s',
        ok: false,
      });
      expect(results[1]).toMatchObject({ confirmedRequests: 2, ok: true });
    });

    it('gives up the request being sent when its signal aborts, and sends no other', async () => {
      const canceler = new AbortController();
      const signals: AbortSignal[] = [];
      const request = vi.fn((_: unknown, options?: { signal?: AbortSignal }) => {
        if (options?.signal) signals.push(options.signal);
        canceler.abort();
        return new Promise<never>(() => undefined);
      });
      const results = await changeUsersInPools(['p1', 'p2'], ['a', 'b', 'c'], request, {
        budget: 24,
        signal: canceler.signal,
      });
      expect(request).toHaveBeenCalledTimes(1);
      expect(signals[0].aborted).toBe(true);
      expect(results).toEqual([expect.objectContaining({ error: 'cancelled', ok: false })]);
    });

    it('sends no more requests once its signal aborts between them', async () => {
      const canceler = new AbortController();
      const calls: string[] = [];
      // Not a mock function, which would answer a tick later than the abort below.
      const request = ({ poolName }: { poolName: string }) => {
        calls.push(poolName);
        const answer = accepted({ poolName });
        // Aborted once the master answered the first request.
        void answer.then(() => canceler.abort());
        return answer;
      };
      const results = await changeUsersInPools(['p1', 'p2'], ['a', 'b', 'c'], request, {
        budget: 24,
        signal: canceler.signal,
      });
      expect(calls).toEqual(['p1']);
      expect(results).toEqual([
        expect.objectContaining({ confirmedRequests: 1, error: 'cancelled', ok: false }),
      ]);
    });
  });

  it('sets the mode of each pool and reports each failure', async () => {
    const request = vi.fn(({ poolName }: { poolName: string }) =>
      poolName === 'bad' ? Promise.reject(new Error('offline')) : accepted({ poolName }),
    );
    const results = await setModeInPools(['good', 'bad'], 'restricted', request);
    expect(request).toHaveBeenCalledWith(
      { mode: 'restricted', poolName: 'good' },
      { signal: expect.any(AbortSignal) },
    );
    expect(results.map((r) => [r.poolName, r.ok, r.error])).toEqual([
      ['good', true, undefined],
      ['bad', false, 'offline'],
    ]);
  });

  it('fails a mode without an answer in time, and sets no more once its signal aborts', async () => {
    const canceler = new AbortController();
    const calls: string[] = [];
    // Not a mock function, which would answer a tick later than the abort below.
    const request = ({ poolName }: { poolName: string }) => {
      calls.push(poolName);
      if (poolName === 'slow') return new Promise<never>(() => undefined);
      const answer = accepted({ poolName });
      void answer.then(() => canceler.abort());
      return answer;
    };
    const results = await setModeInPools(['slow', 'last', 'never'], 'public', request, {
      signal: canceler.signal,
      timeoutMs: 20,
    });
    expect(calls).toEqual(['slow', 'last']);
    expect(results.map((r) => [r.poolName, r.ok, r.error])).toEqual([
      ['slow', false, 'no answer within 0.02 s'],
      ['last', true, undefined],
    ]);
  });

  it('describes errors', () => {
    expect(
      poolAccessErrorMessage(
        new DetError(new Response('', { status: 403 }), { publicMessage: 'no' }),
      ),
    ).toBe('403 no');
    expect(poolAccessErrorMessage(new DetError(undefined, { publicSubject: 'failed' }))).toBe(
      'failed',
    );
    expect(poolAccessErrorMessage('plain')).toBe('plain');
  });
});
