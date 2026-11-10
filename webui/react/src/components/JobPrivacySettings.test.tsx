import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';

import { getJobPrivacyPolicy, getMyJobPrivacy, patchMyJobPrivacy } from 'services/api';
import { V1JobContentKind, V1JobPrivacyRule, V1UserJobPrivacy } from 'services/api-ts-sdk';
import { DetError } from 'utils/error';

import JobPrivacySettings from './JobPrivacySettings';

vi.mock('services/api', () => ({
  getJobPrivacyPolicy: vi.fn(),
  getMyJobPrivacy: vi.fn(),
  patchMyJobPrivacy: vi.fn(),
}));

const allowAll: V1JobPrivacyRule[] = [
  { kind: V1JobContentKind.TITLE, newJobAudience: { allUsers: true }, sharingAllowed: true },
  { kind: V1JobContentKind.LOGS, newJobAudience: { allUsers: true }, sharingAllowed: true },
  { kind: V1JobContentKind.CONFIG, newJobAudience: { allUsers: true }, sharingAllowed: true },
];

const MINE: V1UserJobPrivacy = {
  newJobLogs: { allUsers: false },
  shareLogs: true,
  shareTitles: true,
};

const user = userEvent.setup();

const setup = async () => {
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <JobPrivacySettings />
    </UIProvider>,
  );
  await screen.findByText('Share titles');
};

type Kind = 'titles' | 'logs';
const toggle = (kind: Kind) =>
  within(screen.getByTestId(`job-privacy-${kind}-switch`)).getByRole('switch');
const select = (kind: Kind) => screen.getByTestId(`job-privacy-${kind}-new-jobs`);
const picker = (kind: Kind) => within(select(kind)).getByRole('combobox');
const shown = (kind: Kind) => select(kind).querySelector('.ant-select-selection-item')?.textContent;

const choose = async (kind: Kind, text: string) => {
  await user.click(picker(kind));
  const options = await screen.findAllByText(text, {
    selector:
      '.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option-content',
  });
  await user.click(options[options.length - 1]);
};

const lastPatch = () => vi.mocked(patchMyJobPrivacy).mock.calls.at(-1)?.[0];

describe('JobPrivacySettings', () => {
  beforeEach(() => {
    vi.mocked(getJobPrivacyPolicy).mockReset();
    vi.mocked(getMyJobPrivacy).mockReset();
    vi.mocked(patchMyJobPrivacy).mockReset();
    vi.mocked(getJobPrivacyPolicy).mockResolvedValue(allowAll);
    vi.mocked(getMyJobPrivacy).mockResolvedValue(MINE);
    vi.mocked(patchMyJobPrivacy).mockImplementation((patch) =>
      Promise.resolve({ ...MINE, ...patch }),
    );
  });

  it('shows the switches and the new-job defaults', async () => {
    await setup();
    expect(screen.getByText('Share logs')).toBeInTheDocument();
    expect(screen.getByText('New job titles')).toBeInTheDocument();
    expect(screen.getByText('New job logs')).toBeInTheDocument();
    expect(toggle('titles')).toBeChecked();
    expect(toggle('logs')).toBeChecked();
    // An unset default is the cluster's.
    expect(shown('titles')).toBe('Default');
    expect(shown('logs')).toBe('Only me');
  });

  it('sends only the switch that changed', async () => {
    await setup();
    await user.click(toggle('logs'));
    await waitFor(() => expect(patchMyJobPrivacy).toHaveBeenCalledTimes(1));
    expect(lastPatch()).toEqual({ shareLogs: false });
    await waitFor(() => expect(toggle('logs')).not.toBeChecked());
  });

  it('sends the new-job audience that was chosen', async () => {
    await setup();
    await choose('titles', 'Everyone');
    await waitFor(() => expect(patchMyJobPrivacy).toHaveBeenCalledTimes(1));
    expect(lastPatch()).toEqual({ newJobTitles: { allUsers: true } });
    await choose('logs', 'Everyone');
    await waitFor(() => expect(patchMyJobPrivacy).toHaveBeenCalledTimes(2));
    expect(lastPatch()).toEqual({ newJobLogs: { allUsers: true } });
  });

  it('sends a reset for Default', async () => {
    vi.mocked(patchMyJobPrivacy).mockResolvedValue({ ...MINE, newJobLogs: undefined });
    await setup();
    await choose('logs', 'Default');
    await waitFor(() => expect(patchMyJobPrivacy).toHaveBeenCalledTimes(1));
    expect(lastPatch()).toEqual({ resetNewJobLogs: true });
    await waitFor(() => expect(shown('logs')).toBe('Default'));
  });

  it('shows a kind the policy forbids off and disabled', async () => {
    vi.mocked(getJobPrivacyPolicy).mockResolvedValue([
      { ...allowAll[0], sharingAllowed: false },
      allowAll[1],
      allowAll[2],
    ]);
    await setup();
    expect(toggle('titles')).not.toBeChecked();
    expect(toggle('titles')).toBeDisabled();
    expect(picker('titles')).toBeDisabled();
    expect(toggle('logs')).toBeEnabled();
    expect(picker('logs')).toBeEnabled();
  });

  it('shows the master’s refusal', async () => {
    vi.mocked(patchMyJobPrivacy).mockRejectedValue(
      new DetError(new Error('400'), {
        publicMessage: 'sharing of JOB_CONTENT_KIND_TITLE is forbidden',
      }),
    );
    await setup();
    await choose('titles', 'Everyone');
    expect(
      await screen.findByText('sharing of JOB_CONTENT_KIND_TITLE is forbidden'),
    ).toBeInTheDocument();
    expect(shown('titles')).toBe('Default');
  });
});
