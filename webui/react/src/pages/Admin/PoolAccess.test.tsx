import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { HelmetProvider } from 'react-helmet-async';
import { BrowserRouter } from 'react-router-dom';

import { UNCONFIRMED_NOTE } from 'components/PoolAccessResults';
import { ThemeProvider } from 'components/ThemeProvider';
import { resourcePoolAccessResponse } from 'fixtures/resourcePoolAccess';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import {
  mapResourcePoolAccess,
  mapResourcePoolAccessChange,
  RawResourcePoolAccess,
} from 'services/decoder';
import poolAccessChange from 'stores/poolAccessChange';
import { DetailedUser, ResourcePoolAccessChange, ResourcePoolAccessMode } from 'types';
import { DetError } from 'utils/error';
import { chunkUsernames, PoolAccessResult } from 'utils/resourcePoolAccess';

import PoolAccess, {
  PENDING_DISMISSED_NOTE,
  PUBLIC_INTRO,
  RESTRICT_INTRO,
  RESTRICT_RUNNING_NOTE,
} from './PoolAccess';

const mocks = vi.hoisted(() => ({
  getResourcePoolAccess: vi.fn(),
  grantResourcePoolAccess: vi.fn(),
  handleError: vi.fn(),
  revokeResourcePoolAccess: vi.fn(),
  setResourcePoolAccessMode: vi.fn(),
  users: [] as DetailedUser[],
}));

vi.mock('services/api', () => ({
  getGroup: vi.fn(),
  getGroups: () => Promise.resolve({ groups: [], pagination: { total: 0 } }),
  getResourcePoolAccess: mocks.getResourcePoolAccess,
  getUsers: () =>
    Promise.resolve({ pagination: { total: mocks.users.length }, users: mocks.users }),
  grantResourcePoolAccess: mocks.grantResourcePoolAccess,
  revokeResourcePoolAccess: mocks.revokeResourcePoolAccess,
  setResourcePoolAccessMode: mocks.setResourcePoolAccessMode,
}));

vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: mocks.handleError,
}));

// Table and modal tests render antd components, which is slow when the whole suite runs.
vi.setConfig({ testTimeout: 30_000 });

const pools = () => resourcePoolAccessResponse.resource_pools.map(mapResourcePoolAccess);

const changeOf = (poolName: string, warnings: string[] = []): ResourcePoolAccessChange =>
  mapResourcePoolAccessChange({
    ...resourcePoolAccessResponse.resource_pools.find((pool) => pool.pool_name === poolName),
    warnings,
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

const user = userEvent.setup();

const setup = () =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <DndProvider backend={HTML5Backend}>
          <SettingsProvider>
            <HelmetProvider>
              <BrowserRouter>
                <PoolAccess />
              </BrowserRouter>
            </HelmetProvider>
          </SettingsProvider>
        </DndProvider>
      </ThemeProvider>
    </UIProvider>,
  );

const rowOf = async (poolName: string): Promise<HTMLElement> => {
  const cell = await screen.findByText(poolName, { selector: 'span' });
  const row = cell.closest('tr');
  if (!row) throw new Error(`no row for ${poolName}`);
  return row;
};

const selectPools = async (...poolNames: string[]) => {
  for (const poolName of poolNames) {
    await user.click(within(await rowOf(poolName)).getByRole('checkbox'));
  }
};

const ACTIONS = ['Grant…', 'Revoke…', 'Restrict', 'Make public'];

/** Expands the row of gpu-a100 and selects carol, one of its granted users. */
const selectCarolInDetail = async (): Promise<HTMLElement> => {
  await user.click(within(await rowOf('gpu-a100')).getByRole('button', { name: /expand row/i }));
  const detail = await screen.findByTestId('pool-access-detail-gpu-a100');
  const carol = within(detail).getByText('carol').closest('tr');
  if (!carol) throw new Error('no row for carol');
  await user.click(within(carol).getByRole('checkbox'));
  return within(detail).getByRole('button', { name: 'Revoke selected (1)' });
};

const choose = async (label: string, option: string) => {
  await user.click(screen.getByLabelText(label));
  const options = (await screen.findAllByTitle(option)).filter(
    (element) => !element.closest('.ant-select-dropdown-hidden'),
  );
  await user.click(options[options.length - 1]);
  await user.keyboard('{Escape}');
};

describe('PoolAccess', () => {
  beforeEach(() => {
    mocks.getResourcePoolAccess.mockReset().mockImplementation(() => Promise.resolve(pools()));
    mocks.grantResourcePoolAccess.mockReset();
    mocks.handleError.mockReset();
    mocks.setResourcePoolAccessMode.mockReset();
    mocks.revokeResourcePoolAccess.mockReset();
    mocks.users = [{ id: 7, isActive: true, isAdmin: false, username: 'alice' }];
    // The change of the app outlives the tab: end the one of the last test, also a running one.
    poolAccessChange.reset();
  });

  it('renders each pool of the API response', async () => {
    setup();
    const a100 = await rowOf('gpu-a100');
    expect(within(a100).getByText('Restricted')).toBeInTheDocument();
    expect(within(a100).getByText('cluster default compute')).toBeInTheDocument();
    expect(within(a100).getByTestId('pool-access-users-gpu-a100')).toHaveTextContent(
      '2 (1 inactive)',
    );
    expect(within(a100).getByText('vision (compute)')).toBeInTheDocument();
    // Restricted and the cluster's and a workspace's default compute pool: two warnings.
    expect(within(a100).getByTestId('pool-access-warnings-gpu-a100')).toHaveTextContent('2');

    const cpu = await rowOf('cpu');
    expect(within(cpu).getByText('Public')).toBeInTheDocument();
    expect(within(cpu).getByText('cluster default aux')).toBeInTheDocument();
    // A public default pool refuses nothing, so it has no warning.
    expect(within(cpu).queryByTestId('pool-access-warnings-cpu')).not.toBeInTheDocument();

    const orphan = await rowOf('old-pool');
    expect(within(orphan).getByText('no pool')).toBeInTheDocument();
    expect(within(orphan).getByTestId('pool-access-warnings-old-pool')).toHaveTextContent('1');
  });

  it('filters by pool, granted user, and workspace', async () => {
    setup();
    await rowOf('gpu-a100');
    const search = screen.getByPlaceholderText('Find a pool, granted user, or workspace');

    await user.type(search, 'bob');
    expect(screen.queryByText('gpu-a100', { selector: 'span' })).not.toBeInTheDocument();
    expect(await rowOf('gpu-h100')).toBeInTheDocument();

    await user.clear(search);
    await user.type(search, 'VISION');
    expect(await rowOf('gpu-a100')).toBeInTheDocument();
    expect(screen.queryByText('gpu-h100', { selector: 'span' })).not.toBeInTheDocument();
  });

  it('enables the actions for the selected pools', async () => {
    setup();
    await rowOf('cpu');
    expect(screen.getByRole('button', { name: 'Grant…' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Restrict' })).toBeDisabled();

    await selectPools('cpu', 'gpu-h100');
    expect(screen.getByText('2 pools selected')).toBeInTheDocument();
    for (const name of ['Grant…', 'Revoke…', 'Restrict', 'Make public']) {
      expect(screen.getByRole('button', { name })).toBeEnabled();
    }
  });

  it('explains restricting and shows the result of each pool', async () => {
    mocks.setResourcePoolAccessMode.mockImplementation(({ poolName }) =>
      poolName === 'cpu'
        ? Promise.resolve(
            changeOf('cpu', ['"cpu" is the cluster\'s default aux pool: submissions refused']),
          )
        : Promise.reject(
            new DetError(undefined, { publicMessage: 'database unavailable', publicSubject: 'x' }),
          ),
    );
    setup();
    await selectPools('cpu', 'gpu-a100');
    await user.click(screen.getByRole('button', { name: 'Restrict' }));

    const confirm = await screen.findByTestId('pool-access-restrict-confirm');
    expect(confirm).toHaveTextContent(RESTRICT_INTRO);
    expect(confirm).toHaveTextContent('cpu: no grants: only administrators can use it');
    expect(confirm).toHaveTextContent(
      'gpu-a100: 2 granted users can use it (1 inactive) (already restricted)',
    );
    expect(confirm).toHaveTextContent(RESTRICT_RUNNING_NOTE);
    // What restricting the cluster's default aux pool refuses, before it is restricted.
    expect(confirm).toHaveTextContent(
      '"cpu" is the cluster\'s default aux pool: submissions that omit resources.resource_pool ' +
        'are refused for users without a grant on "cpu"',
    );

    await user.click(screen.getByRole('button', { name: 'Restrict 2 pools' }));
    const results = await screen.findByTestId('pool-access-results');
    expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledTimes(2);
    expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledWith(
      { mode: 'restricted', poolName: 'cpu' },
      { signal: expect.any(AbortSignal) },
    );
    expect(results).toHaveTextContent('1 of 2 pools failed.');
    expect(within(results).getByTestId('pool-access-result-cpu')).toHaveTextContent(
      'cpu: restricted',
    );
    expect(within(results).getByTestId('pool-access-result-cpu')).toHaveTextContent(
      'default aux pool',
    );
    expect(within(results).getByTestId('pool-access-result-gpu-a100')).toHaveTextContent(
      'gpu-a100: failed: database unavailable',
    );
    // The list is read again after the change.
    await waitFor(() => expect(mocks.getResourcePoolAccess).toHaveBeenCalledTimes(2));
  });

  it('says that making a pool public keeps its grants', async () => {
    mocks.setResourcePoolAccessMode.mockImplementation(({ poolName }) =>
      Promise.resolve(changeOf(poolName)),
    );
    setup();
    await selectPools('gpu-a100', 'gpu-h100');
    await user.click(screen.getByRole('button', { name: 'Make public' }));

    const confirm = await screen.findByTestId('pool-access-public-confirm');
    expect(confirm).toHaveTextContent(PUBLIC_INTRO);
    expect(confirm).toHaveTextContent('gpu-a100: 2 grants kept');
    expect(confirm).toHaveTextContent('gpu-h100: already public');
    expect(confirm).toHaveTextContent('apply again when the pool is restricted again');

    await user.click(screen.getByRole('button', { name: 'Make 2 pools public' }));
    expect(await screen.findByText('Done for 2 pools.')).toBeInTheDocument();
    expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledWith(
      { mode: 'public', poolName: 'gpu-a100' },
      { signal: expect.any(AbortSignal) },
    );
  });

  it('revokes the users selected in the detail of a pool', async () => {
    mocks.revokeResourcePoolAccess.mockImplementation(() => Promise.resolve(changeOf('gpu-a100')));
    setup();
    const row = await rowOf('gpu-a100');
    await user.click(within(row).getByRole('button', { name: /expand row/i }));

    const detail = await screen.findByTestId('pool-access-detail-gpu-a100');
    expect(detail).toHaveTextContent('vision: default compute pool');
    expect(detail).toHaveTextContent('Restricted by admin');
    const carol = within(detail).getByText('carol').closest('tr');
    if (!carol) throw new Error('no row for carol');
    expect(carol).toHaveTextContent('inactive');
    await user.click(within(carol).getByRole('checkbox'));
    await user.click(within(detail).getByRole('button', { name: 'Revoke selected (1)' }));

    const confirm = await screen.findByTestId('pool-access-revoke-confirm');
    expect(confirm).toHaveTextContent('Revoke access to gpu-a100 from 1 user: carol.');
    expect(confirm).toHaveTextContent('work that already runs or is queued');
    await user.click(screen.getByRole('button', { name: 'Revoke from 1 user' }));

    expect(await screen.findByText('Done for 1 pool.')).toBeInTheDocument();
    expect(mocks.revokeResourcePoolAccess).toHaveBeenCalledWith(
      { poolName: 'gpu-a100', usernames: ['carol'] },
      { signal: expect.any(AbortSignal) },
    );
  });

  it('shows the results on the tab when the dialog is closed during a change', async () => {
    const cpu = deferred<ResourcePoolAccessChange>();
    mocks.setResourcePoolAccessMode.mockImplementation(({ poolName }) =>
      poolName === 'cpu'
        ? cpu.promise
        : Promise.reject(
            new DetError(undefined, { publicMessage: 'database down', publicSubject: 'x' }),
          ),
    );
    setup();
    await selectPools('cpu', 'gpu-a100');
    await user.click(screen.getByRole('button', { name: 'Restrict' }));
    await user.click(await screen.findByRole('button', { name: 'Restrict 2 pools' }));
    await waitFor(() => expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledTimes(1));

    // The close icon dismisses the dialog while the first request waits.
    await user.click(screen.getByRole('button', { name: 'Close' }));
    await waitFor(() =>
      expect(screen.queryByTestId('pool-access-restrict-confirm')).not.toBeInTheDocument(),
    );
    expect(screen.getByText(PENDING_DISMISSED_NOTE)).toBeInTheDocument();

    cpu.resolve(changeOf('cpu'));
    const dismissed = await screen.findByTestId('pool-access-dismissed-results');
    expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledTimes(2);
    expect(dismissed).toHaveTextContent('Restrict: finished');
    expect(dismissed).toHaveTextContent('1 of 2 pools failed.');
    expect(within(dismissed).getByTestId('pool-access-result-cpu')).toHaveTextContent(
      'cpu: restricted',
    );
    expect(within(dismissed).getByTestId('pool-access-result-gpu-a100')).toHaveTextContent(
      'gpu-a100: failed: database down',
    );
    expect(screen.queryByText(PENDING_DISMISSED_NOTE)).not.toBeInTheDocument();
    // The list is read again after the change.
    await waitFor(() => expect(mocks.getResourcePoolAccess).toHaveBeenCalledTimes(2));
    expect(mocks.handleError).not.toHaveBeenCalled();

    await user.click(within(dismissed).getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByTestId('pool-access-dismissed-results')).not.toBeInTheDocument();
  });

  it('names the failed pools in a notification when the tab is left during a change', async () => {
    const cpu = deferred<ResourcePoolAccessChange>();
    mocks.setResourcePoolAccessMode.mockImplementation(({ poolName }) =>
      poolName === 'cpu'
        ? cpu.promise
        : Promise.reject(
            new DetError(undefined, { publicMessage: 'database down', publicSubject: 'x' }),
          ),
    );
    const { unmount } = setup();
    await selectPools('cpu', 'gpu-a100');
    await user.click(screen.getByRole('button', { name: 'Restrict' }));
    await user.click(await screen.findByRole('button', { name: 'Restrict 2 pools' }));
    await waitFor(() => expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledTimes(1));

    // Admin Settings unmounts the tab when another tab is chosen.
    unmount();
    cpu.resolve(changeOf('cpu'));
    await waitFor(() => expect(mocks.handleError).toHaveBeenCalledTimes(1));
    expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledTimes(2);
    expect(mocks.handleError).toHaveBeenCalledWith(
      expect.any(Error),
      expect.objectContaining({
        publicMessage: 'gpu-a100: failed: database down',
        publicSubject: 'Pool access: Restrict failed for 1 of 2 pools',
        silent: false,
      }),
    );
  });

  it('runs one change at a time: a grant still being sent blocks a newer revoke', async () => {
    const a100 = deferred<ResourcePoolAccessChange>();
    mocks.grantResourcePoolAccess.mockImplementation(({ poolName }) =>
      poolName === 'gpu-a100' ? a100.promise : Promise.resolve(changeOf(poolName)),
    );
    setup();
    await selectPools('gpu-a100', 'gpu-h100');
    await user.click(screen.getByRole('button', { name: 'Grant…' }));
    await choose('Users', 'alice');
    await user.click(await screen.findByRole('button', { name: 'Grant to 1 user' }));
    await waitFor(() => expect(mocks.grantResourcePoolAccess).toHaveBeenCalledTimes(1));

    // The dialog is closed while the grant to gpu-a100 waits; the grant to gpu-h100 is not sent
    // yet, so a revoke from gpu-h100 sent now would be undone by it.
    await user.click(screen.getByRole('button', { name: 'Close' }));
    expect(await screen.findByText(PENDING_DISMISSED_NOTE)).toBeInTheDocument();
    expect(screen.getByText('2 pools selected')).toBeInTheDocument();
    for (const name of ACTIONS) expect(screen.getByRole('button', { name })).toBeDisabled();
    const revokeSelected = await selectCarolInDetail();
    expect(revokeSelected).toBeDisabled();

    a100.resolve(changeOf('gpu-a100'));
    const results = await screen.findByTestId('pool-access-dismissed-results');
    expect(results).toHaveTextContent('Grant: finished');
    expect(results).toHaveTextContent('Done for 2 pools.');
    expect(
      mocks.grantResourcePoolAccess.mock.calls.map(([params]) => [
        params.poolName,
        params.usernames,
      ]),
    ).toEqual([
      ['gpu-a100', ['alice']],
      ['gpu-h100', ['alice']],
    ]);
    for (const name of ACTIONS) expect(screen.getByRole('button', { name })).toBeEnabled();
    expect(revokeSelected).toBeEnabled();
    expect(mocks.revokeResourcePoolAccess).not.toHaveBeenCalled();
  });

  it('keeps the actions waiting when the tab is left and opened again during a change', async () => {
    const cpu = deferred<ResourcePoolAccessChange>();
    mocks.setResourcePoolAccessMode.mockImplementation(() => cpu.promise);
    const first = setup();
    await selectPools('cpu');
    await user.click(screen.getByRole('button', { name: 'Restrict' }));
    await user.click(await screen.findByRole('button', { name: 'Restrict 1 pool' }));
    await waitFor(() => expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledTimes(1));

    // Admin Settings unmounts the tab when another tab is chosen, and mounts it when it is back.
    first.unmount();
    setup();
    await selectPools('cpu', 'gpu-a100');
    for (const name of ACTIONS) expect(screen.getByRole('button', { name })).toBeDisabled();
    expect(screen.getByText(PENDING_DISMISSED_NOTE)).toBeInTheDocument();
    const revokeSelected = await selectCarolInDetail();
    expect(revokeSelected).toBeDisabled();

    cpu.resolve(changeOf('cpu'));
    const results = await screen.findByTestId('pool-access-dismissed-results');
    expect(results).toHaveTextContent('Restrict: finished');
    expect(results).toHaveTextContent('Done for 1 pool.');
    for (const name of ACTIONS) expect(screen.getByRole('button', { name })).toBeEnabled();
    expect(revokeSelected).toBeEnabled();
    // The tab that is shown reads the list again, and the results show there, not in a toast.
    await waitFor(() => expect(mocks.getResourcePoolAccess).toHaveBeenCalledTimes(3));
    expect(mocks.handleError).not.toHaveBeenCalled();
    expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledTimes(1);
  });

  it('does not start a second change from a confirmation being applied', async () => {
    const cpu = deferred<ResourcePoolAccessChange>();
    mocks.setResourcePoolAccessMode.mockImplementation(({ poolName }) =>
      poolName === 'cpu' ? cpu.promise : Promise.resolve(changeOf(poolName)),
    );
    setup();
    await selectPools('cpu', 'gpu-h100');
    await user.click(screen.getByRole('button', { name: 'Restrict' }));
    await user.click(await screen.findByRole('button', { name: 'Restrict 2 pools' }));
    await waitFor(() => expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledTimes(1));

    // hew's Button reads "Loading" while the change is applied.
    const loading = screen.getByRole('button', { name: 'Loading' });
    expect(loading).toBeDisabled();
    fireEvent.click(loading);
    cpu.resolve(changeOf('cpu'));
    expect(await screen.findByText('Done for 2 pools.')).toBeInTheDocument();
    expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledTimes(2);
  });

  it('reads the list again when a change ends, and shows only the latest read', async () => {
    const stale = deferred<ReturnType<typeof pools>>();
    const restricted = pools().map((pool) =>
      pool.poolName === 'cpu' ? { ...pool, mode: ResourcePoolAccessMode.Restricted } : pool,
    );
    mocks.getResourcePoolAccess
      .mockImplementationOnce(() => stale.promise)
      .mockImplementation(() => Promise.resolve(restricted));
    // A change started before the tab is shown, by a dialog of an earlier visit.
    const change = deferred<PoolAccessResult[]>();
    const running = poolAccessChange.run('restrict', () => change.promise);
    setup();
    expect(await screen.findByText(PENDING_DISMISSED_NOTE)).toBeInTheDocument();

    change.resolve([]);
    await running;
    await waitFor(() => expect(mocks.getResourcePoolAccess).toHaveBeenCalledTimes(2));
    expect(within(await rowOf('cpu')).getByText('Restricted')).toBeInTheDocument();
    // The read made when the tab was shown answers last, with the access before the change.
    stale.resolve(pools());
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(within(await rowOf('cpu')).getByText('Restricted')).toBeInTheDocument();
  });

  it('reads the list again when a change throws', async () => {
    const change = deferred<PoolAccessResult[]>();
    const running = poolAccessChange.run('grant', () => change.promise);
    setup();
    await waitFor(() => expect(mocks.getResourcePoolAccess).toHaveBeenCalledTimes(1));

    change.reject(new Error('bug'));
    await expect(running).rejects.toThrow('bug');
    await waitFor(() => expect(mocks.getResourcePoolAccess).toHaveBeenCalledTimes(2));
    expect(screen.queryByText(PENDING_DISMISSED_NOTE)).not.toBeInTheDocument();
    await selectPools('cpu');
    for (const name of ACTIONS) expect(screen.getByRole('button', { name })).toBeEnabled();
  });

  it('says that a failed request may still have been applied, and reads the list again', async () => {
    // Long usernames, so that the grant takes two requests.
    mocks.users = Array.from({ length: 64 }, (_, i) => ({
      id: 100 + i,
      isActive: true,
      isAdmin: false,
      username: `${String(i).padStart(2, '0')}-${'x'.repeat(1000)}`,
    }));
    const usernames = mocks.users.map((u) => u.username);
    const [firstChunk] = chunkUsernames(usernames);
    // The master stores the grants of the first request, and then its answer fails.
    mocks.grantResourcePoolAccess.mockImplementation(() =>
      Promise.reject(
        new DetError(new Response('', { status: 500 }), { publicMessage: 'database unavailable' }),
      ),
    );
    const written = resourcePoolAccessResponse.resource_pools.map(
      (pool): RawResourcePoolAccess =>
        pool.pool_name === 'gpu-h100'
          ? {
              ...pool,
              users: [
                ...(pool.users ?? []),
                ...firstChunk.map((username, i) => ({
                  active: true,
                  admin: false,
                  id: 100 + i,
                  username,
                })),
              ],
            }
          : pool,
    );
    setup();
    await selectPools('gpu-h100');
    mocks.getResourcePoolAccess.mockImplementation(() =>
      Promise.resolve(written.map(mapResourcePoolAccess)),
    );
    await user.click(screen.getByRole('button', { name: 'Grant…' }));
    fireEvent.change(await screen.findByLabelText('Paste usernames'), {
      target: { value: usernames.join('\n') },
    });
    const apply = await screen.findByRole('button', { name: 'Grant to 64 users' });
    await waitFor(() => expect(apply).toBeEnabled());
    await user.click(apply);

    const results = await screen.findByTestId('pool-access-results');
    expect(mocks.grantResourcePoolAccess).toHaveBeenCalledTimes(1);
    expect(within(results).getByTestId('pool-access-result-gpu-h100')).toHaveTextContent(
      'gpu-h100: failed: 500 database unavailable. 0 of 2 requests confirmed (0 of 64 ' +
        'usernames); nothing was retried',
    );
    expect(results).not.toHaveTextContent('were applied');
    expect(results).toHaveTextContent(UNCONFIRMED_NOTE);

    // The list is read again, and shows the grants that the failed request stored.
    await user.click(screen.getAllByRole('button', { name: 'Close' })[0]);
    await waitFor(() =>
      expect(screen.getByTestId('pool-access-users-gpu-h100')).toHaveTextContent(
        String(1 + firstChunk.length),
      ),
    );
    expect(mocks.getResourcePoolAccess).toHaveBeenCalledTimes(2);
  });

  it('shows why the list could not be loaded', async () => {
    mocks.getResourcePoolAccess.mockImplementation(() =>
      Promise.reject(
        new DetError(new Response('', { status: 403 }), { publicMessage: 'access denied' }),
      ),
    );
    setup();
    expect(
      await screen.findByText('Unable to load resource pool access: 403 access denied'),
    ).toBeInTheDocument();
  });
});
