import { render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';
import { createMemoryRouter, RouterProvider } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { getUser } from 'services/api';
import userStore from 'stores/users';
import { DetailedUser } from 'types';
import { DetError } from 'utils/error';

import UserProfile from './UserProfile';

vi.mock('services/api', () => ({
  getUser: vi.fn(),
  getUsers: vi.fn(),
}));

vi.mock('hooks/usePermissions', () => ({ default: () => ({ loading: false }) }));

vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

vi.mock('./UserOverview', () => ({
  default: ({ userId }: { userId: number }) => <div data-testid="user-overview">{userId}</div>,
}));

const ME: DetailedUser = { id: 3, isActive: true, isAdmin: false, username: 'me' };

const ALICE: DetailedUser = {
  displayName: 'Alice Liddell',
  id: 4,
  isActive: true,
  isAdmin: false,
  username: 'alice',
};

const BOB: DetailedUser = { id: 5, isActive: false, isAdmin: false, username: 'bob' };

const notFound = () => new DetError(new Response(null, { status: 404 }));

const setup = (userId: string) => {
  userStore.updateCurrentUser(ME);
  const router = createMemoryRouter(
    [
      { element: <UserProfile />, path: '/users/:userId' },
      { element: <div>Home page</div>, path: '/dashboard' },
    ],
    { initialEntries: [`/users/${userId}`] },
  );
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <HelmetProvider>
          <RouterProvider router={router} />
        </HelmetProvider>
      </ThemeProvider>
    </UIProvider>,
  );
  return router;
};

describe('UserProfile', () => {
  beforeEach(() => {
    vi.mocked(getUser).mockImplementation(({ userId }) => {
      const user = [ALICE, BOB].find((user) => user.id === userId);
      return user ? Promise.resolve(user) : Promise.reject(notFound());
    });
  });

  afterEach(() => {
    vi.clearAllMocks();
    userStore.reset();
  });

  it("shows another user's badge, a link to the user's jobs and the overview, without Home's sections", async () => {
    setup('4');
    expect(await screen.findByText('Alice Liddell')).toBeInTheDocument();
    expect(screen.getByText('alice')).toBeInTheDocument();
    expect(screen.queryByText('Deactivated')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Jobs' })).toHaveAttribute('href', '/jobs?user=4');
    expect(screen.getByTestId('user-overview')).toHaveTextContent(/^4$/);
    expect(screen.queryByText('Recently Viewed Projects')).not.toBeInTheDocument();
    expect(screen.queryByText('Your Recent Submissions')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Launch Interactive Task' })).toBeNull();
    // The user is read by id.
    expect(getUser).toHaveBeenCalledWith({ userId: 4 }, expect.anything());
  });

  it('marks a deactivated user', async () => {
    setup('5');
    expect(await screen.findByText('Deactivated')).toBeInTheDocument();
  });

  it("redirects the signed-in user's own page to Home", async () => {
    const router = setup('3');
    expect(await screen.findByText('Home page')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/dashboard');
    expect(router.state.historyAction).toBe('REPLACE');
    expect(getUser).not.toHaveBeenCalled();
  });

  it('loads before it shows the user', async () => {
    vi.mocked(getUser).mockReturnValue(new Promise(() => undefined));
    setup('4');
    await waitFor(() => expect(getUser).toHaveBeenCalled());
    expect(screen.queryByTestId('user-overview')).not.toBeInTheDocument();
    expect(screen.queryByText('User not found')).not.toBeInTheDocument();
    expect(document.querySelector('.ant-spin-spinning')).not.toBeNull();
  });

  it('says "User not found" for an unknown id', async () => {
    setup('99');
    expect(await screen.findByText('User not found')).toBeInTheDocument();
    expect(screen.queryByTestId('user-overview')).not.toBeInTheDocument();
  });

  it.each(['alice', '4x'])('says "User not found" for /users/%s without a request', async (id) => {
    setup(id);
    expect(await screen.findByText('User not found')).toBeInTheDocument();
    expect(getUser).not.toHaveBeenCalled();
  });

  it('says when the user cannot be read', async () => {
    vi.mocked(getUser).mockRejectedValue(new DetError(new Response(null, { status: 500 })));
    setup('4');
    expect(await screen.findByText('Unable to fetch user')).toBeInTheDocument();
    expect(screen.queryByText('User not found')).not.toBeInTheDocument();
    expect(screen.queryByTestId('user-overview')).not.toBeInTheDocument();
  });
});
