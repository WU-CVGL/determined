import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';

import { getJobPrivacyPolicy, putJobPrivacyPolicy } from 'services/api';
import { V1JobContentKind, V1JobPrivacyRule } from 'services/api-ts-sdk';
import { DetError } from 'utils/error';

import JobPrivacy from './JobPrivacy';

vi.mock('services/api', () => ({
  getJobPrivacyPolicy: vi.fn(),
  putJobPrivacyPolicy: vi.fn(),
}));

const POLICY: V1JobPrivacyRule[] = [
  { kind: V1JobContentKind.TITLE, newJobAudience: { allUsers: true }, sharingAllowed: true },
  { kind: V1JobContentKind.LOGS, newJobAudience: { allUsers: true }, sharingAllowed: true },
  { kind: V1JobContentKind.CONFIG, newJobAudience: { allUsers: false }, sharingAllowed: false },
];

const user = userEvent.setup();

const setup = async () => {
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <JobPrivacy />
    </UIProvider>,
  );
  await screen.findByTestId(`job-privacy-sharing-${V1JobContentKind.TITLE}`);
};

const cell = (column: 'sharing' | 'new-jobs', kind: V1JobContentKind) =>
  screen.getByTestId(`job-privacy-${column}-${kind}`);
const shown = (column: 'sharing' | 'new-jobs', kind: V1JobContentKind) =>
  cell(column, kind).querySelector('.ant-select-selection-item')?.textContent;

const choose = async (column: 'sharing' | 'new-jobs', kind: V1JobContentKind, text: string) => {
  await user.click(within(cell(column, kind)).getByRole('combobox'));
  const options = await screen.findAllByText(text, {
    selector:
      '.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option-content',
  });
  await user.click(options[options.length - 1]);
};

describe('Admin Job Privacy', () => {
  beforeEach(() => {
    vi.mocked(getJobPrivacyPolicy).mockReset();
    vi.mocked(putJobPrivacyPolicy).mockReset();
    vi.mocked(getJobPrivacyPolicy).mockResolvedValue(POLICY);
  });

  it('shows the policy of each kind', async () => {
    await setup();
    ['Content', 'Sharing', 'New jobs', 'Titles', 'Logs', 'Config'].forEach((text) =>
      expect(screen.getByText(text)).toBeInTheDocument(),
    );
    expect(shown('sharing', V1JobContentKind.TITLE)).toBe('Allowed');
    expect(shown('new-jobs', V1JobContentKind.TITLE)).toBe('Everyone');
    expect(shown('sharing', V1JobContentKind.CONFIG)).toBe('Forbidden');
    expect(shown('new-jobs', V1JobContentKind.CONFIG)).toBe('Owner only');
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
  });

  it('shows a kind without a rule as forbidden', async () => {
    vi.mocked(getJobPrivacyPolicy).mockResolvedValue(POLICY.slice(0, 2));
    await setup();
    expect(shown('sharing', V1JobContentKind.CONFIG)).toBe('Forbidden');
    expect(shown('new-jobs', V1JobContentKind.CONFIG)).toBe('Owner only');
  });

  it('saves the whole policy in one request', async () => {
    const saved: V1JobPrivacyRule[] = [
      POLICY[0],
      { kind: V1JobContentKind.LOGS, newJobAudience: { allUsers: false }, sharingAllowed: false },
      POLICY[2],
    ];
    vi.mocked(putJobPrivacyPolicy).mockResolvedValue(saved);
    await setup();
    await choose('sharing', V1JobContentKind.LOGS, 'Forbidden');
    await choose('new-jobs', V1JobContentKind.LOGS, 'Owner only');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(putJobPrivacyPolicy).toHaveBeenCalledTimes(1));
    expect(vi.mocked(putJobPrivacyPolicy).mock.calls[0][0]).toEqual({
      rules: [
        { kind: V1JobContentKind.TITLE, newJobAudience: { allUsers: true }, sharingAllowed: true },
        { kind: V1JobContentKind.LOGS, newJobAudience: { allUsers: false }, sharingAllowed: false },
        {
          kind: V1JobContentKind.CONFIG,
          newJobAudience: { allUsers: false },
          sharingAllowed: false,
        },
      ],
    });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled());
    expect(shown('sharing', V1JobContentKind.LOGS)).toBe('Forbidden');
  });

  it('shows the master’s text when the save fails', async () => {
    vi.mocked(putJobPrivacyPolicy).mockRejectedValue(
      new DetError(new Error('403'), { publicMessage: 'only admins set the job privacy policy' }),
    );
    await setup();
    await choose('sharing', V1JobContentKind.TITLE, 'Forbidden');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('only admins set the job privacy policy')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();
  });

  it('shows the master’s text when the policy cannot be read', async () => {
    vi.mocked(getJobPrivacyPolicy).mockRejectedValue(
      new DetError(new Error('503'), {
        publicMessage: 'could not read the job privacy policy: down; try again',
      }),
    );
    render(
      <UIProvider theme={DefaultTheme.Light}>
        <JobPrivacy />
      </UIProvider>,
    );
    expect(
      await screen.findByText('could not read the job privacy policy: down; try again'),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
  });
});
