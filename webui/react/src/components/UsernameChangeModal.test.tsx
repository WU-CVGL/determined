import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import Button from 'hew/Button';
import { useModal } from 'hew/Modal';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import React from 'react';

import { patchUser as mockPatchUser } from 'services/api';
import userStore from 'stores/users';
import { DetailedUser } from 'types';
import { DetError } from 'utils/error';

import UsernameChangeModalComponent, {
  API_SUCCESS_MESSAGE,
  CURRENT_PASSWORD_LABEL,
  INCORRECT_PASSWORD_MESSAGE,
  OK_BUTTON_LABEL,
} from './UsernameChangeModal';

const OPEN_MODAL_TEXT = 'Open Modal';
const NEW_USERNAME = 'new_name';
const CURRENT_USER: DetailedUser = { id: 4, isActive: true, isAdmin: false, username: 'old_name' };

vi.mock('services/api', () => ({ patchUser: vi.fn() }));

const user = userEvent.setup();

const Container: React.FC<{ onClose?: () => void; onSubmit: () => void }> = ({
  onClose,
  onSubmit,
}) => {
  const UsernameChangeModal = useModal(UsernameChangeModalComponent);
  return (
    <div>
      <Button onClick={UsernameChangeModal.open}>{OPEN_MODAL_TEXT}</Button>
      <UsernameChangeModal.Component
        newUsername={NEW_USERNAME}
        onClose={onClose}
        onSubmit={onSubmit}
      />
    </div>
  );
};

const setup = async (onSubmit = vi.fn(), onClose?: () => void) => {
  const view = render(
    <UIProvider theme={DefaultTheme.Light}>
      <Container onClose={onClose} onSubmit={onSubmit} />
    </UIProvider>,
  );
  await user.click(await view.findByText(OPEN_MODAL_TEXT));
  return view;
};

describe('Username Change Modal', () => {
  beforeEach(() => {
    vi.mocked(mockPatchUser).mockReset();
    userStore.updateCurrentUser(CURRENT_USER);
  });
  afterEach(() => userStore.reset());

  it('renames the current user with their current password', async () => {
    vi.mocked(mockPatchUser).mockResolvedValue({ ...CURRENT_USER, username: NEW_USERNAME });
    const onSubmit = vi.fn();
    await setup(onSubmit);

    await user.type(screen.getByLabelText(CURRENT_PASSWORD_LABEL), 'Current-1');
    await user.click(screen.getByRole('button', { name: OK_BUTTON_LABEL }));

    expect(await screen.findByText(API_SUCCESS_MESSAGE)).toBeInTheDocument();
    expect(mockPatchUser).toHaveBeenCalledWith({
      userId: CURRENT_USER.id,
      userParams: { oldPassword: 'Current-1', username: NEW_USERNAME },
    });
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it('sends an empty current password for users without one', async () => {
    vi.mocked(mockPatchUser).mockResolvedValue({ ...CURRENT_USER, username: NEW_USERNAME });
    await setup();

    await user.click(screen.getByRole('button', { name: OK_BUTTON_LABEL }));

    await waitFor(() =>
      expect(mockPatchUser).toHaveBeenCalledWith({
        userId: CURRENT_USER.id,
        userParams: { oldPassword: '', username: NEW_USERNAME },
      }),
    );
  });

  it('marks the current password incorrect when the master refuses it', async () => {
    // The master answers 403 Forbidden for a wrong current password.
    vi.mocked(mockPatchUser).mockRejectedValue(new DetError(new Response(null, { status: 403 })));
    const onSubmit = vi.fn();
    await setup(onSubmit);

    await user.type(screen.getByLabelText(CURRENT_PASSWORD_LABEL), 'wrong');
    await user.click(screen.getByRole('button', { name: OK_BUTTON_LABEL }));

    expect(await screen.findByText(INCORRECT_PASSWORD_MESSAGE)).toBeInTheDocument();
    // The modal stays open for another try.
    expect(screen.getByLabelText(CURRENT_PASSWORD_LABEL)).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('tells the caller when it closes without renaming the user', async () => {
    const onClose = vi.fn();
    const onSubmit = vi.fn();
    await setup(onSubmit, onClose);

    await user.click(screen.getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(mockPatchUser).not.toHaveBeenCalled();
  });
});
