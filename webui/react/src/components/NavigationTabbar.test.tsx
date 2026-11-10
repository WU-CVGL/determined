import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import authStore from 'stores/auth';
import userStore from 'stores/users';

import NavigationTabbar from './NavigationTabbar';

vi.mock('services/api', () => ({
  getUserSetting: () => Promise.resolve({ settings: [] }),
  getWorkspaces: () => Promise.resolve({ pagination: { total: 0 }, workspaces: [] }),
}));

vi.mock('components/WorkspaceCreateModal', () => ({ default: () => null }));

vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canAdministrateUsers: false, canCreateWorkspace: false, loading: false }),
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
            <NavigationTabbar />
          </BrowserRouter>
        </ConfirmationProvider>
      </ThemeProvider>
    </UIProvider>,
  );
};

describe('NavigationTabbar', () => {
  it('has no My Profile entry in the overflow menu', async () => {
    const user = userEvent.setup();
    setup();
    await user.click(screen.getAllByLabelText('Overflow Menu')[0]);
    const labels = screen
      .getAllByText(/^(My Profile|User Settings|Sign out)$/)
      .map((el) => el.textContent);
    expect(labels).toEqual(['User Settings', 'Sign out']);
  });
});
