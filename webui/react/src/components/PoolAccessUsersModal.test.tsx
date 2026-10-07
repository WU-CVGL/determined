import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import Button from 'hew/Button';
import { useModal } from 'hew/Modal';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { Loadable } from 'hew/utils/loadable';
import React from 'react';

import { PoolAccessRunner } from 'components/PoolAccessResults';
import { ThemeProvider } from 'components/ThemeProvider';
import { resourcePoolAccessResponse } from 'fixtures/resourcePoolAccess';
import { mapResourcePoolAccess, mapResourcePoolAccessChange } from 'services/decoder';
import userStore from 'stores/users';
import { DetailedUser, ResourcePoolAccess } from 'types';
import { DetError } from 'utils/error';
import {
  PoolAccessUsersAction,
  RESOURCE_POOL_ACCESS_MAX_BODY_BYTES,
  usernamesBodyBytes,
} from 'utils/resourcePoolAccess';

import PoolAccessUsersModalComponent, {
  GRANT_GROUP_NOTE,
  MEMBERSHIP_CHANGED_MESSAGE,
  REVOKE_GROUP_NOTE,
} from './PoolAccessUsersModal';

const OPEN = 'Open';

const mocks = vi.hoisted(() => ({
  getGroup: vi.fn(),
  grantResourcePoolAccess: vi.fn(),
  revokeResourcePoolAccess: vi.fn(),
  users: [] as DetailedUser[],
}));

vi.mock('services/api', () => ({
  getGroup: mocks.getGroup,
  getGroups: () =>
    Promise.resolve({
      groups: [
        { group: { groupId: 1, name: 'team-a' }, numMembers: 2 },
        { group: { groupId: 2, name: 'team-b' }, numMembers: 2 },
      ],
      pagination: { total: 2 },
    }),
  getUsers: () =>
    Promise.resolve({ pagination: { total: mocks.users.length }, users: mocks.users }),
  grantResourcePoolAccess: mocks.grantResourcePoolAccess,
  revokeResourcePoolAccess: mocks.revokeResourcePoolAccess,
}));

// Table and modal tests render antd components, which is slow when the whole suite runs.
vi.setConfig({ testTimeout: 15_000 });

const baseUsers: DetailedUser[] = [
  { id: 1, isActive: true, isAdmin: false, username: 'alice' },
  { id: 2, isActive: true, isAdmin: false, username: 'bob' },
  { id: 3, isActive: false, isAdmin: false, username: 'carol' },
  { id: 4, isActive: true, isAdmin: true, username: 'dave' },
  { id: 5, isActive: true, isAdmin: false, username: 'john doe' },
];

const groupResponse = (groupId: number, usernames: string[]) => ({
  group: {
    groupId,
    name: `team-${groupId}`,
    users: usernames.map((username) => ({ active: true, admin: false, username })),
  },
});

const allPools = resourcePoolAccessResponse.resource_pools.map(mapResourcePoolAccess);
const poolsNamed = (...names: string[]): ResourcePoolAccess[] =>
  allPools.filter((pool) => names.includes(pool.poolName));

const accepted = ({ poolName }: { poolName: string }) =>
  Promise.resolve(mapResourcePoolAccessChange({ exists: true, pool_name: poolName }));

interface ContainerProps {
  action: PoolAccessUsersAction;
  pools: ResourcePoolAccess[];
  runChange: PoolAccessRunner;
}

const Container: React.FC<ContainerProps> = ({ action, pools, runChange }) => {
  const UsersModal = useModal(PoolAccessUsersModalComponent);
  return (
    <div>
      <Button onClick={UsersModal.open}>{OPEN}</Button>
      <UsersModal.Component
        action={action}
        closeModal={() => UsersModal.close('cancel')}
        pools={pools}
        runChange={runChange}
      />
    </div>
  );
};

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

const setup = async (pools: ResourcePoolAccess[], action: PoolAccessUsersAction = 'grant') => {
  const runChange = vi.fn<Parameters<PoolAccessRunner>, ReturnType<PoolAccessRunner>>(
    (_action, run) => run(),
  );
  userStore.fetchUsers();
  await waitFor(() =>
    expect(Loadable.getOrElse([], userStore.getUsers().get())).toHaveLength(mocks.users.length),
  );
  const view = render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <Container action={action} pools={pools} runChange={runChange} />
      </ThemeProvider>
    </UIProvider>,
  );
  await user.click(screen.getByText(OPEN));
  return { runChange, unmount: view.unmount };
};

const choose = async (label: string, option: string) => {
  await user.click(screen.getByLabelText(label));
  const options = (await screen.findAllByTitle(option)).filter(
    (element) => !element.closest('.ant-select-dropdown-hidden'),
  );
  await user.click(options[options.length - 1]);
  await user.keyboard('{Escape}');
};

const paste = (text: string) =>
  fireEvent.change(screen.getByLabelText('Paste usernames'), { target: { value: text } });

const preview = () => screen.getByTestId('pool-access-preview');

describe('PoolAccessUsersModal', () => {
  beforeEach(() => {
    mocks.users = baseUsers;
    mocks.getGroup.mockReset();
    mocks.grantResourcePoolAccess.mockReset().mockImplementation(accepted);
    mocks.revokeResourcePoolAccess.mockReset().mockImplementation(accepted);
  });

  it('combines users, groups, and pasted names, and refuses unknown names', async () => {
    mocks.getGroup.mockImplementation(({ groupId }) =>
      Promise.resolve(groupResponse(groupId, ['bob', 'alice'])),
    );
    const { runChange } = await setup(poolsNamed('gpu-a100', 'gpu-h100'));

    expect(screen.getByText(GRANT_GROUP_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(REVOKE_GROUP_NOTE)).not.toBeInTheDocument();
    expect(screen.getByTestId('pool-access-modal-pools')).toHaveTextContent(
      'Grant access to gpu-a100, gpu-h100',
    );
    // gpu-h100 is public: its grants have no effect until it is restricted.
    expect(screen.getByText(/gpu-h100 is public: the grants have no effect/)).toBeInTheDocument();

    await choose('Users', 'alice');
    await choose('Groups', 'team-b (2 members)');
    paste('carol, alice\nzed\njohn doe\ndave');

    await waitFor(() => expect(preview()).toHaveTextContent('5 users after removing duplicates'));
    expect(preview()).toHaveTextContent('(1 picked, 2 from groups, 5 pasted), 1 inactive');
    expect(preview()).toHaveTextContent('1 administrator, who may use every pool anyway');
    expect(preview()).toHaveTextContent('carol (inactive)');
    expect(preview()).toHaveTextContent('dave (admin)');
    expect(preview()).toHaveTextContent('john doe');
    expect(screen.getByText('1 unknown username: remove it to continue.', { exact: false }));
    expect(screen.getByText('zed')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Grant to 5 users' })).toBeDisabled();

    paste('carol, alice\njohn doe\ndave');
    const apply = screen.getByRole('button', { name: 'Grant to 5 users' });
    await waitFor(() => expect(apply).toBeEnabled());
    expect(mocks.getGroup).toHaveBeenCalledTimes(1);
    await user.click(apply);

    expect(await screen.findByTestId('pool-access-results')).toHaveTextContent('Done for 2 pools.');
    // The group is expanded again when the grant is applied.
    expect(mocks.getGroup).toHaveBeenCalledTimes(2);
    const usernames = ['alice', 'bob', 'carol', 'dave', 'john doe'];
    expect(mocks.grantResourcePoolAccess).toHaveBeenCalledTimes(2);
    expect(mocks.grantResourcePoolAccess).toHaveBeenCalledWith({ poolName: 'gpu-a100', usernames });
    expect(mocks.grantResourcePoolAccess).toHaveBeenCalledWith({ poolName: 'gpu-h100', usernames });
    expect(screen.getByTestId('pool-access-result-gpu-a100')).toHaveTextContent(
      'gpu-a100: grant applied for 5 usernames',
    );
    // The counts are the usernames sent; the master does not say whose access changed.
    expect(screen.getByTestId('pool-access-results')).toHaveTextContent(
      'A user who already had a grant, or had none to revoke, is counted but unchanged.',
    );
    // The change goes through the tab's runner, which keeps its results if the modal is closed.
    expect(runChange).toHaveBeenCalledTimes(1);
    expect(runChange).toHaveBeenCalledWith('grant', expect.any(Function));
  });

  it('stops when a group changed between the preview and applying', async () => {
    mocks.getGroup
      .mockImplementationOnce(({ groupId }) => Promise.resolve(groupResponse(groupId, ['alice'])))
      .mockImplementation(({ groupId }) =>
        Promise.resolve(groupResponse(groupId, ['alice', 'bob'])),
      );
    await setup(poolsNamed('gpu-a100'));

    await choose('Groups', 'team-a (2 members)');
    await waitFor(() => expect(preview()).toHaveTextContent('1 user after removing duplicates'));
    await user.click(screen.getByRole('button', { name: 'Grant to 1 user' }));

    expect(await screen.findByText(MEMBERSHIP_CHANGED_MESSAGE)).toBeInTheDocument();
    expect(mocks.grantResourcePoolAccess).not.toHaveBeenCalled();
    expect(preview()).toHaveTextContent('2 users after removing duplicates');

    await user.click(screen.getByRole('button', { name: 'Grant to 2 users' }));
    await screen.findByTestId('pool-access-results');
    expect(mocks.grantResourcePoolAccess).toHaveBeenCalledWith({
      poolName: 'gpu-a100',
      usernames: ['alice', 'bob'],
    });
  });

  it('splits large changes under the body limit and reports each pool', async () => {
    const many: DetailedUser[] = Array.from({ length: 2500 }, (_, i) => ({
      id: 100 + i,
      isActive: true,
      isAdmin: false,
      username: `user-${String(i).padStart(4, '0')}-with-a-long-name`,
    }));
    mocks.users = many;
    let h100Requests = 0;
    mocks.grantResourcePoolAccess.mockImplementation(({ poolName }) => {
      if (poolName === 'gpu-h100' && ++h100Requests === 2) {
        return Promise.reject(
          new DetError(new Response('', { status: 500 }), {
            publicMessage: 'database unavailable',
            publicSubject: 'Request grantResourcePoolAccess failed.',
          }),
        );
      }
      return accepted({ poolName });
    });
    await setup(poolsNamed('gpu-a100', 'gpu-h100'));

    paste(many.map((u) => u.username).join('\n'));
    const apply = await screen.findByRole('button', { name: 'Grant to 2500 users' });
    await waitFor(() => expect(apply).toBeEnabled());
    await user.click(apply);
    const results = await screen.findByTestId('pool-access-results');

    const a100Calls = mocks.grantResourcePoolAccess.mock.calls
      .map(([params]) => params)
      .filter((params) => params.poolName === 'gpu-a100');
    expect(a100Calls.length).toBeGreaterThan(1);
    for (const params of a100Calls) {
      expect(usernamesBodyBytes(params.usernames)).toBeLessThanOrEqual(
        RESOURCE_POOL_ACCESS_MAX_BODY_BYTES,
      );
    }
    expect(a100Calls.flatMap((params) => params.usernames)).toEqual(
      many.map((u) => u.username).sort((a, b) => a.localeCompare(b)),
    );
    // gpu-h100 stops at its failed request; nothing is retried.
    expect(h100Requests).toBe(2);

    expect(results).toHaveTextContent('1 of 2 pools failed.');
    expect(within(results).getByTestId('pool-access-result-gpu-a100')).toHaveTextContent(
      'gpu-a100: grant applied for 2500 usernames',
    );
    const firstChunk = a100Calls[0].usernames.length;
    expect(within(results).getByTestId('pool-access-result-gpu-h100')).toHaveTextContent(
      `gpu-h100: failed: 500 database unavailable. 1 of ${a100Calls.length} requests ` +
        `confirmed (${firstChunk} of 2500 usernames); nothing was retried`,
    );
  });

  it('locks the picker while a change is applied', async () => {
    const grant = deferred<ReturnType<typeof mapResourcePoolAccessChange>>();
    mocks.grantResourcePoolAccess.mockImplementation(() => grant.promise);
    await setup(poolsNamed('gpu-a100'));

    await choose('Users', 'alice');
    paste('bob');
    await user.click(screen.getByRole('button', { name: 'Grant to 2 users' }));
    await waitFor(() => expect(mocks.grantResourcePoolAccess).toHaveBeenCalledTimes(1));
    // Edits made now would change the preview but not the change being sent.
    expect(screen.getByLabelText('Users')).toBeDisabled();
    expect(screen.getByLabelText('Groups')).toBeDisabled();
    expect(screen.getByLabelText('Paste usernames')).toBeDisabled();

    grant.resolve(mapResourcePoolAccessChange({ exists: true, pool_name: 'gpu-a100' }));
    expect(await screen.findByTestId('pool-access-results')).toHaveTextContent('Done for 1 pool.');
  });

  it('sends nothing when closed while the groups are expanded for applying', async () => {
    const second = deferred<ReturnType<typeof groupResponse>>();
    mocks.getGroup
      .mockImplementationOnce(({ groupId }) => Promise.resolve(groupResponse(groupId, ['alice'])))
      .mockImplementationOnce(() => second.promise);
    const { unmount } = await setup(poolsNamed('gpu-a100'));

    await choose('Groups', 'team-a (2 members)');
    await waitFor(() => expect(preview()).toHaveTextContent('1 user after removing duplicates'));
    await user.click(screen.getByRole('button', { name: 'Grant to 1 user' }));
    await waitFor(() => expect(mocks.getGroup).toHaveBeenCalledTimes(2));

    // The Pool Access tab unmounts the modal when it is closed.
    unmount();
    second.resolve(groupResponse(1, ['alice']));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(mocks.grantResourcePoolAccess).not.toHaveBeenCalled();
  });

  it('ignores a group expansion that failed after its group was removed', async () => {
    const expansion = deferred<ReturnType<typeof groupResponse>>();
    mocks.getGroup.mockImplementation(() => expansion.promise);
    await setup(poolsNamed('gpu-a100'));

    await choose('Users', 'alice');
    await choose('Groups', 'team-a (2 members)');
    await waitFor(() => expect(mocks.getGroup).toHaveBeenCalledTimes(1));
    expect(preview()).toHaveTextContent('Expanding groups...');
    // Choosing the group again removes it, while its expansion still waits.
    await choose('Groups', 'team-a (2 members)');
    await waitFor(() => expect(preview()).toHaveTextContent('1 user after removing duplicates'));

    expansion.reject(new DetError(undefined, { publicMessage: 'group service down' }));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByText(/Unable to expand the groups/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Grant to 1 user' })).toBeEnabled();
  });

  it('reports the master refusing a pool', async () => {
    mocks.revokeResourcePoolAccess.mockImplementation(({ poolName }) =>
      poolName === 'gpu-a100'
        ? Promise.reject(
            new DetError(new Response('', { status: 404 }), {
              publicMessage: 'unknown users: bob; nothing was changed',
            }),
          )
        : accepted({ poolName }),
    );
    await setup(poolsNamed('cpu', 'gpu-a100'), 'revoke');

    expect(screen.getByText(/Revoking applies from the next request/)).toBeInTheDocument();
    expect(screen.getByText(REVOKE_GROUP_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(GRANT_GROUP_NOTE)).not.toBeInTheDocument();
    await choose('Users', 'bob');
    await user.click(screen.getByRole('button', { name: 'Revoke from 1 user' }));

    const results = await screen.findByTestId('pool-access-results');
    expect(results).toHaveTextContent('1 of 2 pools failed.');
    expect(within(results).getByTestId('pool-access-result-cpu')).toHaveTextContent(
      'cpu: revoke applied for 1 username',
    );
    expect(within(results).getByTestId('pool-access-result-gpu-a100')).toHaveTextContent(
      'gpu-a100: failed: 404 unknown users: bob; nothing was changed',
    );
    expect(mocks.revokeResourcePoolAccess).toHaveBeenCalledTimes(2);
  });
});
