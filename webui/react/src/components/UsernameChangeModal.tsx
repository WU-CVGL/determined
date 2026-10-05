import Form from 'hew/Form';
import Input from 'hew/Input';
import { Modal } from 'hew/Modal';
import { useToast } from 'hew/Toast';
import { Loadable } from 'hew/utils/loadable';
import React, { useId } from 'react';

import userStore from 'stores/users';
import handleError, { ErrorType, isDetError } from 'utils/error';
import { useObservable } from 'utils/observable';
import { getResponseStatus } from 'utils/service';

const MODAL_HEADER_LABEL = 'Change Username';
export const CURRENT_PASSWORD_LABEL = 'Current Password';
const CURRENT_PASSWORD_NAME = 'currentPassword';
export const OK_BUTTON_LABEL = 'Change Username';
export const INCORRECT_PASSWORD_MESSAGE = 'Incorrect password.';
export const API_SUCCESS_MESSAGE = 'Username updated.';
export const API_ERROR_MESSAGE = 'Could not update username.';

const FORM_ID = 'edit-username-form';

interface FormInputs {
  [CURRENT_PASSWORD_NAME]: string;
}

interface Props {
  newUsername: string;
  /** Called when the modal closes without renaming the user. */
  onClose?: () => void;
  onSubmit?: () => void;
}

/*
 * Renames the current user after asking for their current password. Users sign in with their
 * username, so the master needs the current password to rename yourself, as it does to change your
 * password: a stolen session could otherwise lock you out.
 */
const UsernameChangeModalComponent: React.FC<Props> = ({
  newUsername,
  onClose,
  onSubmit,
}: Props) => {
  const idPrefix = useId();
  const [form] = Form.useForm<FormInputs>();
  const currentUser = Loadable.getOrElse(undefined, useObservable(userStore.currentUser));
  const { openToast } = useToast();

  const handleSubmit = async () => {
    try {
      // Users without a password leave it blank.
      const oldPassword: string = form.getFieldValue(CURRENT_PASSWORD_NAME) ?? '';
      await userStore.patchUser(currentUser?.id ?? 0, { oldPassword, username: newUsername });
      openToast({ severity: 'Confirm', title: API_SUCCESS_MESSAGE });
      form.resetFields();
      onSubmit?.();
    } catch (e) {
      const status = isDetError(e) ? getResponseStatus(e.sourceErr) : getResponseStatus(e);
      if (status === 403) {
        form.setFields([{ errors: [INCORRECT_PASSWORD_MESSAGE], name: CURRENT_PASSWORD_NAME }]);
      }
      openToast({ severity: 'Error', title: API_ERROR_MESSAGE });
      handleError(e, { silent: true, type: ErrorType.Input });

      // Re-throw error to prevent modal from getting dismissed.
      throw e;
    }
  };

  return (
    <Modal
      cancel
      size="small"
      submit={{
        form: idPrefix + FORM_ID,
        handleError,
        handler: handleSubmit,
        text: OK_BUTTON_LABEL,
      }}
      title={MODAL_HEADER_LABEL}
      onClose={() => {
        form.resetFields();
        onClose?.();
      }}>
      <p>Enter your current password to change your username to {newUsername}.</p>
      <Form form={form} id={idPrefix + FORM_ID}>
        <Form.Item label={CURRENT_PASSWORD_LABEL} name={CURRENT_PASSWORD_NAME}>
          <Input.Password autoFocus />
        </Form.Item>
      </Form>
    </Modal>
  );
};

export default UsernameChangeModalComponent;
