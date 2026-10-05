import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import Button from 'hew/Button';
import { useModal } from 'hew/Modal';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { Loaded } from 'hew/utils/loadable';
import React from 'react';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { postUser as mockCreateUser, patchUser as mockPatchUser } from 'services/api';
import userStore from 'stores/users';
import { DetailedUser } from 'types';

import CreateUserModalComponent, {
  API_SUCCESS_MESSAGE_CREATE,
  BUTTON_NAME,
  DISPLAY_NAME_LABEL,
  MODAL_HEADER_LABEL_CREATE,
  OWN_PASSWORD_MESSAGE,
  USER_NAME_LABEL,
  USER_PASSWORD_CONFIRM_LABEL,
  USER_PASSWORD_LABEL,
} from './CreateUserModal';

vi.mock('services/api', () => ({
  getCurrentUser: vi.fn().mockResolvedValue({}),
  patchUser: vi.fn().mockResolvedValue({}),
  postUser: vi.fn().mockReturnValue({ user: { id: 1 } }),
}));

const ADMIN: DetailedUser = { id: 7, isActive: true, isAdmin: true, username: 'admin' };
const OTHER: DetailedUser = { id: 8, isActive: true, isAdmin: false, username: 'other' };

const OPEN_MODAL_TEXT = 'Open Modal';
const USERNAME = 'test_username1';
const PASSWORD = 'test_Password1';

const user = userEvent.setup();

const Container: React.FC<{ editUser?: DetailedUser }> = ({ editUser }) => {
  const CreateUserModal = useModal(CreateUserModalComponent);

  return (
    <div>
      <Button onClick={CreateUserModal.open}>{OPEN_MODAL_TEXT}</Button>
      <CreateUserModal.Component user={editUser} userRoles={editUser && Loaded([])} />
    </div>
  );
};

const setup = async (editUser?: DetailedUser) => {
  const view = render(
    <BrowserRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <Container editUser={editUser} />
        </ThemeProvider>
      </UIProvider>
    </BrowserRouter>,
  );

  await user.click(await view.findByText(OPEN_MODAL_TEXT));
  await view.findByText(editUser ? 'Edit User' : MODAL_HEADER_LABEL_CREATE);

  // Check for the modal to finish loading.
  await waitFor(() => {
    expect(screen.queryByText('Loading', { exact: false })).not.toBeInTheDocument();
  });

  return view;
};

describe('Create User Modal', () => {
  it('should submit a valid create user request', async () => {
    await setup();

    await user.type(screen.getByLabelText(USER_NAME_LABEL), USERNAME);
    await user.type(screen.getByLabelText(USER_PASSWORD_LABEL), PASSWORD);
    await user.type(screen.getByLabelText(USER_PASSWORD_CONFIRM_LABEL), PASSWORD);
    await user.click(screen.getByRole('button', { name: BUTTON_NAME }));

    // Check for successful toast message.
    await waitFor(() => {
      expect(
        screen.getByText(API_SUCCESS_MESSAGE_CREATE, { collapseWhitespace: false }),
      ).toBeInTheDocument();
    });

    // Check that the API method was called with the correct parameters.
    expect(mockCreateUser).toHaveBeenCalledWith({
      password: PASSWORD,
      user: { active: true, username: USERNAME },
    });
  });

  describe('editing a user', () => {
    beforeEach(() => {
      vi.mocked(mockPatchUser).mockClear();
      userStore.updateCurrentUser(ADMIN);
    });
    afterEach(() => userStore.reset());

    it('leaves out the password when admins edit themselves', async () => {
      await setup(ADMIN);

      // The master needs the current password to change your own, which this form does not ask.
      expect(screen.queryByLabelText(USER_PASSWORD_LABEL)).not.toBeInTheDocument();
      expect(screen.queryByLabelText(USER_PASSWORD_CONFIRM_LABEL)).not.toBeInTheDocument();
      expect(screen.getByText(OWN_PASSWORD_MESSAGE)).toBeInTheDocument();

      await user.type(screen.getByLabelText(DISPLAY_NAME_LABEL), 'Admin Name');
      await user.click(screen.getByRole('button', { name: BUTTON_NAME }));

      await waitFor(() => expect(mockPatchUser).toHaveBeenCalledTimes(1));
      const { userId, userParams } = vi.mocked(mockPatchUser).mock.calls[0][0];
      expect(userId).toBe(ADMIN.id);
      expect(userParams.displayName).toBe('Admin Name');
      expect(userParams).not.toHaveProperty('password');
      // Nor is the username changed, which would need the current password too.
      expect([undefined, ADMIN.username]).toContain(userParams.username);
    });

    it("sets another user's password without a current password", async () => {
      await setup(OTHER);

      expect(screen.queryByText(OWN_PASSWORD_MESSAGE)).not.toBeInTheDocument();
      await user.type(screen.getByLabelText(USER_PASSWORD_LABEL), PASSWORD);
      await user.type(screen.getByLabelText(USER_PASSWORD_CONFIRM_LABEL), PASSWORD);
      await user.click(screen.getByRole('button', { name: BUTTON_NAME }));

      await waitFor(() => expect(mockPatchUser).toHaveBeenCalledTimes(1));
      const { userId, userParams } = vi.mocked(mockPatchUser).mock.calls[0][0];
      expect(userId).toBe(OTHER.id);
      expect(userParams.password).toBe(PASSWORD);
      expect(userParams).not.toHaveProperty('oldPassword');
    });
  });
});
