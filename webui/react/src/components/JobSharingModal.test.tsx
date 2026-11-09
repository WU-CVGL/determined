import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import React from 'react';

import { getJobSharing, patchJobSharing } from 'services/api';
import { V1JobContentKind, V1JobPrivacyRule } from 'services/api-ts-sdk';
import { JobSharingSettings } from 'types';
import { DetError } from 'utils/error';

import { useJobSharingModal } from './JobSharingModal';

vi.mock('services/api', () => ({
  getJobSharing: vi.fn(),
  patchJobSharing: vi.fn(),
}));

const allowAll: V1JobPrivacyRule[] = [
  { kind: V1JobContentKind.TITLE, newJobAudience: { allUsers: true }, sharingAllowed: true },
  { kind: V1JobContentKind.LOGS, newJobAudience: { allUsers: true }, sharingAllowed: true },
  { kind: V1JobContentKind.CONFIG, newJobAudience: { allUsers: true }, sharingAllowed: true },
];

const settings = (
  sharing: JobSharingSettings['sharing'],
  rules: V1JobPrivacyRule[] = allowAll,
): JobSharingSettings => ({ rules, sharing });

const Harness: React.FC<{ onSaved?: () => void }> = ({ onSaved }) => {
  const { modal, open } = useJobSharingModal(onSaved);
  return (
    <>
      <button onClick={() => open('job-1')}>open</button>
      {modal}
    </>
  );
};

const user = userEvent.setup();

const setup = async (onSaved?: () => void) => {
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <Harness onSaved={onSaved} />
    </UIProvider>,
  );
  await user.click(screen.getByText('open'));
  await screen.findByTestId('job-sharing-title');
};

const row = (field: 'title' | 'logs' | 'config') => screen.getByTestId(`job-sharing-${field}`);
const picker = (field: 'title' | 'logs' | 'config') => within(row(field)).getByRole('combobox');
const shown = (field: 'title' | 'logs' | 'config') =>
  row(field).querySelector('.ant-select-selection-item')?.textContent;

/** The option of the open dropdown with this text. */
const openOption = async (text: string): Promise<HTMLElement> => {
  const options = await screen.findAllByText(text, {
    selector:
      '.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option-content',
  });
  return options[options.length - 1].closest('.ant-select-item-option') as HTMLElement;
};

const choose = async (field: 'title' | 'logs' | 'config', text: string) => {
  await user.click(picker(field));
  await user.click(await openOption(text));
};

describe('JobSharingModal', () => {
  beforeEach(() => {
    vi.mocked(getJobSharing).mockReset();
    vi.mocked(patchJobSharing).mockReset();
    vi.mocked(patchJobSharing).mockResolvedValue({});
  });

  it('reads the sharing only when it opens', async () => {
    vi.mocked(getJobSharing).mockResolvedValue(settings({}));
    render(
      <UIProvider theme={DefaultTheme.Light}>
        <Harness />
      </UIProvider>,
    );
    expect(getJobSharing).not.toHaveBeenCalled();
    await user.click(screen.getByText('open'));
    await screen.findByTestId('job-sharing-title');
    expect(getJobSharing).toHaveBeenCalledTimes(1);
    expect(vi.mocked(getJobSharing).mock.calls[0][0]).toEqual({ jobId: 'job-1' });
  });

  it('shows each kind, an unset audience as Everyone', async () => {
    vi.mocked(getJobSharing).mockResolvedValue(
      settings({ config: { allUsers: true }, logs: { allUsers: false } }),
    );
    await setup();
    expect(screen.getByText('Sharing')).toBeInTheDocument();
    expect(within(row('title')).getByText('Title')).toBeInTheDocument();
    expect(within(row('logs')).getByText('Logs')).toBeInTheDocument();
    expect(within(row('config')).getByText('Config')).toBeInTheDocument();
    expect(shown('title')).toBe('Everyone');
    expect(shown('logs')).toBe('Only me');
    expect(shown('config')).toBe('Everyone');
    expect(screen.getByRole('button', { name: 'Save' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
  });

  it('disables Everyone for a kind the policy forbids', async () => {
    vi.mocked(getJobSharing).mockResolvedValue(
      settings({}, [allowAll[0], { ...allowAll[1], sharingAllowed: false }, allowAll[2]]),
    );
    await setup();
    await user.click(picker('logs'));
    expect(await openOption('Everyone')).toHaveClass('ant-select-item-option-disabled');
    expect(await openOption('Only me')).not.toHaveClass('ant-select-item-option-disabled');
  });

  it('disables Logs and Config while Title is Only me', async () => {
    vi.mocked(getJobSharing).mockResolvedValue(settings({ title: { allUsers: true } }));
    await setup();
    expect(picker('logs')).toBeEnabled();
    await choose('title', 'Only me');
    expect(picker('logs')).toBeDisabled();
    expect(picker('config')).toBeDisabled();
    // Their choices are kept.
    expect(shown('logs')).toBe('Everyone');
  });

  it('sends only the changed kinds and calls back after the save', async () => {
    vi.mocked(getJobSharing).mockResolvedValue(
      settings({ config: { allUsers: true }, logs: { allUsers: true }, title: { allUsers: true } }),
    );
    const onSaved = vi.fn();
    await setup(onSaved);
    await choose('config', 'Only me');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    expect(patchJobSharing).toHaveBeenCalledTimes(1);
    expect(vi.mocked(patchJobSharing).mock.calls[0][0]).toEqual({
      config: { allUsers: false },
      jobId: 'job-1',
    });
    await waitFor(() => expect(screen.queryByTestId('job-sharing-title')).not.toBeInTheDocument());
  });

  it('shows the master’s refusal and stays open', async () => {
    vi.mocked(getJobSharing).mockResolvedValue(settings({ title: { allUsers: false } }));
    vi.mocked(patchJobSharing).mockRejectedValue(
      new DetError(new Error('400'), {
        publicMessage: 'sharing of JOB_CONTENT_KIND_TITLE is forbidden',
        publicSubject: 'Request patchJobSharing failed.',
      }),
    );
    const onSaved = vi.fn();
    await setup(onSaved);
    await choose('title', 'Everyone');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(
      await screen.findByText('sharing of JOB_CONTENT_KIND_TITLE is forbidden'),
    ).toBeInTheDocument();
    expect(row('title')).toBeInTheDocument();
    expect(onSaved).not.toHaveBeenCalled();
  });

  it('shows the master’s text when the sharing cannot be read', async () => {
    vi.mocked(getJobSharing).mockRejectedValue(
      new DetError(new Error('403'), {
        publicMessage: 'only the job’s owner and admins manage its sharing',
      }),
    );
    render(
      <UIProvider theme={DefaultTheme.Light}>
        <Harness />
      </UIProvider>,
    );
    await user.click(screen.getByText('open'));
    expect(
      await screen.findByText('only the job’s owner and admins manage its sharing'),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
  });
});
