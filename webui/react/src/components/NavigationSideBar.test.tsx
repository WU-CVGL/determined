import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import authStore from 'stores/auth';
import userStore from 'stores/users';

import NavigationSideBar from './NavigationSideBar';

vi.mock('services/api', () => ({
  getUserSetting: () => Promise.resolve({ settings: [] }),
  getWorkspaces: () => Promise.resolve({ pagination: { total: 0 }, workspaces: [] }),
}));

vi.mock('components/WorkspaceCreateModal', () => ({ default: () => null }));

vi.mock('hooks/usePermissions', () => ({
  default: () => ({
    canAdministrateUsers: false,
    canCreateWorkspace: false,
    canViewGlobalConfigPolicies: false,
    canViewWorkspace: () => true,
    loading: false,
  }),
}));

const ME = { id: 3, isActive: true, isAdmin: false, username: 'me' };

const setup = () => {
  authStore.setAuth({ isAuthenticated: true });
  authStore.setAuthChecked();
  userStore.updateCurrentUser(ME);
  return render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <ConfirmationProvider>
          <BrowserRouter>
            <NavigationSideBar />
          </BrowserRouter>
        </ConfirmationProvider>
      </ThemeProvider>
    </UIProvider>,
  );
};

describe('NavigationSideBar', () => {
  afterEach(() => {
    window.history.replaceState(null, '', '/');
  });

  it("opens the signed-in user's page from My Profile, the first item of the user menu", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(screen.getByTestId('headerDropdown'));
    const items = await screen.findAllByRole('menuitem');
    expect(items[0]).toHaveTextContent('My Profile');
    const link = screen.getByText('My Profile').closest('a') as HTMLElement;
    expect(link).toHaveAttribute('href', '/users/3');
    await user.click(link);
    expect(window.location.pathname).toBe('/users/3');
  });
});
