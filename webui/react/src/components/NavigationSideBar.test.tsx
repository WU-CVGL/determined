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
  it('has no My Profile entry in the user menu', async () => {
    const user = userEvent.setup();
    setup();
    await user.click(screen.getByTestId('headerDropdown'));
    const items = await screen.findAllByRole('menuitem');
    expect(items.length).toBeGreaterThan(0);
    expect(screen.queryByText('My Profile')).not.toBeInTheDocument();
  });
});
