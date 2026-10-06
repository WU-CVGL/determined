import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import React, { useCallback, useEffect } from 'react';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { HelmetProvider } from 'react-helmet-async';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import authStore from 'stores/auth';
import determinedStore from 'stores/determinedInfo';
import userStore from 'stores/users';
import { DetailedUser } from 'types';
import { WritableObservable } from 'utils/observable';

import Admin from '.';

const DISPLAY_NAME = 'Test Name';
const USERNAME = 'test_username1';

const CURRENT_USER: DetailedUser = {
  displayName: DISPLAY_NAME,
  id: 1,
  isActive: true,
  isAdmin: true,
  username: USERNAME,
};

const mocks = vi.hoisted(() => {
  return {
    canAdministrateUsers: false,
    canAssignRoles: vi.fn(),
    canManageResourcePoolAccess: false,
    canViewGroups: false,
    listRoles: vi.fn(() => Promise.resolve([])),
  };
});

vi.mock('stores/determinedInfo', async (importOriginal) => {
  const observable = await import('utils/observable');
  const store = {
    info: observable.observable({
      rbacEnabled: true,
    }),
  };
  return {
    ...(await importOriginal<typeof import('stores/determinedInfo')>()),
    default: store,
  };
});

vi.mock('hooks/usePermissions', () => {
  const usePermissions = vi.fn(() => {
    return {
      canAdministrateUsers: mocks.canAdministrateUsers,
      canAssignRoles: mocks.canAssignRoles,
      canManageResourcePoolAccess: mocks.canManageResourcePoolAccess,
      canViewGroups: mocks.canViewGroups,
    };
  });
  return {
    default: usePermissions,
  };
});

vi.mock('services/api', () => ({
  getGroup: () => Promise.resolve({ group: { users: [] } }),
  getGroupRoles: () => Promise.resolve([]),
  getGroups: () =>
    Promise.resolve({
      groups: [],
      pagination: {
        endIndex: 10,
        limit: 0,
        offset: 0,
        startIndex: 0,
        total: 10,
      },
    }),
  getResourcePoolAccess: () => Promise.resolve([]),
  getUsers: () => {
    const users: Array<DetailedUser> = [CURRENT_USER];
    return Promise.resolve({ pagination: { total: 1 }, users });
  },
  grantResourcePoolAccess: vi.fn(),
  listRoles: mocks.listRoles,
  revokeResourcePoolAccess: vi.fn(),
  setResourcePoolAccessMode: vi.fn(),
}));

const Container: React.FC = () => {
  const loadUsers = useCallback(() => {
    userStore.fetchUsers();
    authStore.setAuth({ isAuthenticated: true });
    authStore.setAuthChecked();
    userStore.updateCurrentUser(CURRENT_USER);
  }, []);

  useEffect(() => {
    loadUsers();
  }, [loadUsers]);

  return (
    <SettingsProvider>
      <HelmetProvider>
        <BrowserRouter>
          <Admin />;
        </BrowserRouter>
      </HelmetProvider>
    </SettingsProvider>
  );
};

const setRbacEnabled = (rbacEnabled: boolean) => {
  const info = determinedStore.info as unknown as WritableObservable<{ rbacEnabled: boolean }>;
  act(() => info.set({ ...info.get(), rbacEnabled }));
};

const user = userEvent.setup();

const setup = () =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <DndProvider backend={HTML5Backend}>
          <Container />;
        </DndProvider>
      </ThemeProvider>
    </UIProvider>,
  );

describe('Admin page', () => {
  beforeEach(() => {
    setRbacEnabled(true);
    mocks.listRoles.mockClear();
    mocks.canAdministrateUsers = false;
    mocks.canManageResourcePoolAccess = false;
    mocks.canViewGroups = false;
  });

  it('should hide users tab without permissions', () => {
    setup();
    expect(screen.getByText('Admin Settings')).toBeInTheDocument();
    expect(screen.queryByText('Users')).not.toBeInTheDocument();
  });

  it('should render users tab with permissions', async () => {
    mocks.canAdministrateUsers = true;
    setup();
    expect(screen.getByText('Admin Settings')).toBeInTheDocument();
    expect(await screen.findByText('Users (1)')).toBeInTheDocument();
  });

  it('shows the Groups tab when groups may be viewed, also without RBAC', async () => {
    setRbacEnabled(false);
    mocks.canAdministrateUsers = true;
    mocks.canViewGroups = true;
    setup();
    expect(await screen.findByText('Users (1)')).toBeInTheDocument();
    await user.click(screen.getByText(/^Groups/));

    // Group management works without RBAC and asks for no roles.
    expect(await screen.findByRole('button', { name: 'New Group' })).toBeInTheDocument();
    expect(mocks.listRoles).not.toHaveBeenCalled();
  });

  it('hides the Groups tab when groups may not be viewed', async () => {
    mocks.canAdministrateUsers = true;
    setup();
    expect(await screen.findByText('Users (1)')).toBeInTheDocument();
    expect(screen.queryByText(/^Groups/)).not.toBeInTheDocument();
  });

  it('shows the Pool Access tab only to users who may update the master configuration', async () => {
    mocks.canAdministrateUsers = true;
    const { unmount } = setup();
    expect(await screen.findByText('Users (1)')).toBeInTheDocument();
    expect(screen.queryByText('Pool Access')).not.toBeInTheDocument();
    unmount();

    mocks.canManageResourcePoolAccess = true;
    setup();
    expect(await screen.findByText('Pool Access')).toBeInTheDocument();
  });
});
