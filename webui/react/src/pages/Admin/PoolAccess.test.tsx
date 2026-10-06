import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { HelmetProvider } from 'react-helmet-async';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { resourcePoolAccessResponse } from 'fixtures/resourcePoolAccess';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import { mapResourcePoolAccess, mapResourcePoolAccessChange } from 'services/decoder';
import { ResourcePoolAccessChange } from 'types';
import { DetError } from 'utils/error';

import PoolAccess, { PUBLIC_INTRO, RESTRICT_INTRO, RESTRICT_RUNNING_NOTE } from './PoolAccess';

const mocks = vi.hoisted(() => ({
  getResourcePoolAccess: vi.fn(),
  revokeResourcePoolAccess: vi.fn(),
  setResourcePoolAccessMode: vi.fn(),
}));

vi.mock('services/api', () => ({
  getGroup: vi.fn(),
  getGroups: () => Promise.resolve({ groups: [], pagination: { total: 0 } }),
  getResourcePoolAccess: mocks.getResourcePoolAccess,
  getUsers: () => Promise.resolve({ pagination: { total: 0 }, users: [] }),
  grantResourcePoolAccess: vi.fn(),
  revokeResourcePoolAccess: mocks.revokeResourcePoolAccess,
  setResourcePoolAccessMode: mocks.setResourcePoolAccessMode,
}));

// Table and modal tests render antd components, which is slow when the whole suite runs.
vi.setConfig({ testTimeout: 15_000 });

const pools = () => resourcePoolAccessResponse.resource_pools.map(mapResourcePoolAccess);

const changeOf = (poolName: string, warnings: string[] = []): ResourcePoolAccessChange =>
  mapResourcePoolAccessChange({
    ...resourcePoolAccessResponse.resource_pools.find((pool) => pool.pool_name === poolName),
    warnings,
  });

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

describe('PoolAccess', () => {
  beforeEach(() => {
    mocks.getResourcePoolAccess.mockReset().mockImplementation(() => Promise.resolve(pools()));
    mocks.setResourcePoolAccessMode.mockReset();
    mocks.revokeResourcePoolAccess.mockReset();
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
    expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledWith({
      mode: 'restricted',
      poolName: 'cpu',
    });
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
    expect(mocks.setResourcePoolAccessMode).toHaveBeenCalledWith({
      mode: 'public',
      poolName: 'gpu-a100',
    });
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
    expect(mocks.revokeResourcePoolAccess).toHaveBeenCalledWith({
      poolName: 'gpu-a100',
      usernames: ['carol'],
    });
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
