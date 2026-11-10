import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { MemoryRouter } from 'react-router-dom';

import { killTask } from 'services/api';
import { CommandState, CommandType } from 'types';
import { isDangerMenuItem, isDisabledMenuItem, menuLabels } from 'utils/tests/menu';

import TaskBar from './TaskBar';

const OWNER = { userId: 7, workspaceId: 1 };

vi.mock('services/api', () => ({ killTask: vi.fn() }));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canModifyWorkspaceNSC: ({ userId }: { userId?: number }) => userId === 7 }),
}));
vi.mock('utils/routes', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/routes')>()),
  routeToReactUrl: vi.fn(),
}));

const renderBar = (
  props: Partial<React.ComponentProps<typeof TaskBar>> = {},
  handleViewLogsClick = vi.fn(),
) =>
  render(
    <MemoryRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ConfirmationProvider>
          <TaskBar
            handleViewLogsClick={handleViewLogsClick}
            id="cmd-1"
            name="Command (lively-calm-fox)"
            owner={OWNER}
            resourcePool="default"
            state={CommandState.Running}
            type={CommandType.Command}
            {...props}
          />
        </ConfirmationProvider>
      </UIProvider>
    </MemoryRouter>,
  );

describe('TaskBar', () => {
  beforeEach(() => {
    vi.mocked(killTask).mockReset();
  });

  it('lists View Logs first and Kill last, in red, like the task action menu', async () => {
    const handleViewLogsClick = vi.fn();
    renderBar({}, handleViewLogsClick);
    expect(screen.getByText('default')).toBeInTheDocument();
    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await screen.findByText('Kill');
    expect(menuLabels()).toEqual(['View Logs', 'Kill']);
    expect(isDangerMenuItem('Kill')).toBe(true);
    expect(isDangerMenuItem('View Logs')).toBe(false);
    expect(isDisabledMenuItem('Kill')).toBe(false);
    await userEvent.click(screen.getByText('View Logs'));
    expect(handleViewLogsClick).toHaveBeenCalledTimes(1);
  });

  it('offers Sharing between View Logs and Kill when given', async () => {
    const onSharing = vi.fn();
    renderBar({ onSharing });
    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await screen.findByText('Kill');
    expect(menuLabels()).toEqual(['View Logs', 'Sharing', 'Kill']);
    await userEvent.click(screen.getByText('Sharing'));
    expect(onSharing).toHaveBeenCalledTimes(1);
  });

  it('kills the task by its type after a confirmation', async () => {
    renderBar();
    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await userEvent.click(await screen.findByText('Kill'));
    await userEvent.click(await screen.findByRole('button', { name: 'Kill' }));
    await waitFor(() =>
      expect(killTask).toHaveBeenCalledWith({ id: 'cmd-1', type: CommandType.Command }),
    );
  });

  it.each([
    ['a task of an unknown type', { type: undefined }],
    ['a task whose owner is unknown', { owner: undefined }],
    ['a task the user may not modify', { owner: { userId: 8, workspaceId: 1 } }],
    ['a task that has ended', { state: CommandState.Terminated }],
    ['a task whose state is unknown', { state: undefined }],
  ])('keeps View Logs and leaves Kill disabled for %s', async (_, props) => {
    const handleViewLogsClick = vi.fn();
    renderBar(props, handleViewLogsClick);
    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await screen.findByText('Kill');
    expect(menuLabels()).toEqual(['View Logs', 'Kill']);
    expect(isDisabledMenuItem('Kill')).toBe(true);
    await userEvent.click(screen.getByText('View Logs'));
    expect(handleViewLogsClick).toHaveBeenCalledTimes(1);
  });

  it('shows the name alone when the pool is unknown', () => {
    renderBar({ resourcePool: '' });
    expect(screen.getByText('Command (lively-calm-fox)')).toBeInTheDocument();
    expect(screen.queryByText('—')).not.toBeInTheDocument();
  });
});
