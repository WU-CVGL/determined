import { waitFor } from '@testing-library/dom';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { useInitApi } from 'hew/Toast';
import { ConfirmationProvider } from 'hew/useConfirm';
import React, { useCallback, useEffect } from 'react';

import { ThemeProvider } from 'components/ThemeProvider';
import { patchUser as mockPatchUser } from 'services/api';
import { PatchUserParams } from 'services/types';
import authStore from 'stores/auth';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';
import { DetailedUser } from 'types';

import { CURRENT_PASSWORD_LABEL, OK_BUTTON_LABEL } from './UsernameChangeModal';
import UserSettings from './UserSettings';

vi.mock('services/api', () => ({
  getUsers: () =>
    Promise.resolve({
      users: [
        {
          displayName: 'Test Name',
          id: 1,
          isActive: true,
          isAdmin: false,
          username: 'test_username1',
        },
      ],
    }),
  patchUser: vi.fn((params: PatchUserParams) =>
    Promise.resolve({
      displayName: params.userParams.displayName,
      id: 1,
      isActive: true,
    }),
  ),
}));

vi.mock('routes/utils', async (importOriginal) => ({
  ...(await importOriginal<typeof import('routes/utils')>()),
  serverAddress: () => 'http://localhost',
}));

const user = userEvent.setup();

const DISPLAY_NAME = 'Test Name';
const USERNAME = 'test_username1';

const CURRENT_USER: DetailedUser = {
  displayName: DISPLAY_NAME,
  id: 1,
  isActive: true,
  isAdmin: false,
  username: USERNAME,
};

const Container: React.FC<{ currentUser: DetailedUser }> = ({ currentUser }) => {
  const loadUsers = useCallback(() => {
    userStore.updateCurrentUser(currentUser);
  }, [currentUser]);

  useEffect(() => {
    authStore.setAuth({ isAuthenticated: true });
    userSettings.startPolling();
    return userStore.fetchUsers();
  }, []);

  useEffect(() => {
    loadUsers();
  }, [loadUsers]);

  useInitApi();
  return (
    <UserSettings
      show={true}
      onClose={() => {
        return null;
      }}
    />
  );
};

const setup = (currentUser = CURRENT_USER) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <ConfirmationProvider>
          <Container currentUser={currentUser} />
        </ConfirmationProvider>
      </ThemeProvider>
    </UIProvider>,
  );

describe('UserSettings', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('should render with correct values', async () => {
    setup();
    expect(await screen.findByText('Username')).toBeInTheDocument();
    expect(screen.getByText('Display Name')).toBeInTheDocument();
    expect(screen.getByText('Password')).toBeInTheDocument();
    expect(await screen.findByText(USERNAME)).toBeInTheDocument();
  });
  it('should be able to change display name', async () => {
    setup();
    await user.click(screen.getByTestId('edit-displayname'));
    await user.type(screen.getByPlaceholderText('Add display name'), 'a');
    await user.click(screen.getByTestId('submit-displayname'));
    expect(mockPatchUser).toHaveBeenCalledWith({
      userId: 1,
      userParams: { displayName: `${DISPLAY_NAME}a` },
    });
    await waitFor(() =>
      expect(screen.getByTestId('value-displayname')).toHaveTextContent(`${DISPLAY_NAME}a`),
    );
  });
  it('asks for the current password before renaming yourself', async () => {
    setup();
    await user.click(await screen.findByTestId('edit-username'));
    await user.type(screen.getByPlaceholderText('Add username'), 'a');
    await user.click(screen.getByTestId('submit-username'));

    // The master needs the current password to rename yourself.
    await user.type(await screen.findByLabelText(CURRENT_PASSWORD_LABEL), 'Current-1');
    expect(mockPatchUser).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: OK_BUTTON_LABEL }));

    await waitFor(() =>
      expect(mockPatchUser).toHaveBeenCalledWith({
        userId: 1,
        userParams: { oldPassword: 'Current-1', username: `${USERNAME}a` },
      }),
    );
  });
  it('does not offer remote users a new username', async () => {
    setup({ ...CURRENT_USER, remote: true });
    expect(await screen.findByTestId('edit-username')).toBeDisabled();
  });
});
