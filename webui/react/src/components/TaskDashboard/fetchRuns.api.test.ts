import { DateString } from 'ioTypes';
import { Taskv1State, V1Shell } from 'services/api-ts-sdk';

import { fetchRunPage, RunQuery } from './fetchRuns';
import { RunKind } from './runRows';

// setupTests mocks services/api for every test; these tests go through the real API wrappers.
vi.unmock('services/api');

/* 1001 shells: the last by ID is also the newest. */
const SHELL_COUNT = 1001;
const shellId = (i: number) => `shell-${String(i).padStart(4, '0')}`;
const SHELLS: V1Shell[] = Array.from({ length: SHELL_COUNT }, (_, i) => ({
  description: `shell ${i}`,
  id: shellId(i),
  jobId: `job-${i}`,
  resourcePool: 'default',
  slots: 0,
  startTime: new Date(Date.UTC(2026, 0, 1) + i * 60_000).toISOString() as DateString,
  state: Taskv1State.RUNNING,
  username: 'alice',
  workspaceId: 1,
}));
const LAST_SHELL_ID = shellId(SHELL_COUNT - 1);

/**
 * The master's GET /api/v1/shells: sorted by ID, then paged by the offset and limit of the query
 * (api.Sort, api.Paginate), where a limit of 0 or none is all of them.
 */
const fakeMaster = (input: RequestInfo | URL): Promise<Response> => {
  const url = new URL(String(input), 'http://master');
  if (!url.pathname.endsWith('/api/v1/shells')) throw new Error(`unexpected request: ${url}`);
  const offset = Number(url.searchParams.get('offset') ?? 0);
  const limit = Number(url.searchParams.get('limit') ?? 0);
  const shells = [...SHELLS]
    .sort((a, b) => a.id.localeCompare(b.id))
    .slice(offset, limit === 0 ? undefined : offset + limit);
  return Promise.resolve(new Response(JSON.stringify({ shells }), { status: 200 }));
};

const shellsOnly = (overrides: Partial<RunQuery> = {}): RunQuery => ({
  kinds: [RunKind.Shell],
  limit: 20,
  offset: 0,
  pageKinds: [RunKind.Shell],
  scope: { type: 'global' },
  ...overrides,
});

describe('fetchRunPage against the list API', () => {
  beforeEach(() => {
    vi.spyOn(window, 'fetch').mockImplementation(fakeMaster);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('lists every shell, also those after the first 1000 by ID', async () => {
    const page = await fetchRunPage(shellsOnly());

    expect(page.errors).toEqual({});
    expect(page.total).toBe(SHELL_COUNT);
    expect(page.rows[0].id).toBe(LAST_SHELL_ID);
  });

  it('finds a shell after the first 1000 by ID by its ID', async () => {
    const page = await fetchRunPage(shellsOnly({ search: LAST_SHELL_ID }));

    expect(page.errors).toEqual({});
    expect(page.rows.map((row) => row.id)).toEqual([LAST_SHELL_ID]);
    expect(page.total).toBe(1);
  });
});
