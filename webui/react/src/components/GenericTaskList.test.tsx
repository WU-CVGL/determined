import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import { getGenericTasks, killGenericTask, unpauseGenericTask } from 'services/api';
import userStore from 'stores/users';
import { GenericTask, GenericTaskState } from 'types';
import handleError from 'utils/error';
import { isDangerMenuItem, isDisabledMenuItem, menuLabels, openMenuItem } from 'utils/tests/menu';

import GenericTaskList from './GenericTaskList';

const CURRENT_USER_ID = 3;

/* The state the list API reports for the task. */
const listed = vi.hoisted(() => ({ state: 'ACTIVE' }));

const TASK: GenericTask = {
  description: '',
  jobId: 'job-1',
  name: 'eval-sweep',
  noPause: false,
  projectId: 1,
  resourcePool: 'default',
  slots: 1,
  startTime: '2026-01-01T00:00:00Z',
  state: GenericTaskState.Active,
  taskId: 'task-1',
  userId: 4,
  username: 'bob',
  workspaceId: 7,
};

vi.mock('services/api', () => ({
  getGenericTasks: vi.fn(() =>
    Promise.resolve({
      pagination: { limit: 10, offset: 0, total: 1 },
      tasks: [{ ...TASK, state: listed.state }],
    }),
  ),
  killGenericTask: vi.fn(() => Promise.resolve()),
  pauseGenericTask: vi.fn(() => Promise.resolve()),
  unpauseGenericTask: vi.fn(() => Promise.resolve()),
}));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canModifyWorkspaceNSC: () => true }),
}));
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => true }));
vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

const FULL_MENU = ['View Logs', 'View Resources', 'Copy Task ID', 'Pause', 'Kill'];

const setup = (workspaceId?: number) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <DndProvider backend={HTML5Backend}>
          <SettingsProvider>
            <BrowserRouter>
              <ConfirmationProvider>
                <GenericTaskList workspaceId={workspaceId} />
              </ConfirmationProvider>
            </BrowserRouter>
          </SettingsProvider>
        </DndProvider>
      </ThemeProvider>
    </UIProvider>,
  );

describe('GenericTaskList', () => {
  beforeAll(() => {
    userStore.updateCurrentUser({
      id: CURRENT_USER_ID,
      isActive: true,
      isAdmin: false,
      username: 'alice',
    });
  });

  beforeEach(() => {
    listed.state = GenericTaskState.Active;
  });

  afterEach(() => vi.clearAllMocks());

  it("lists all users' tasks of a workspace", async () => {
    setup(7);
    expect(await screen.findByText('eval-sweep')).toBeInTheDocument();
    await waitFor(() => expect(getGenericTasks).toHaveBeenCalled());
    const [params] = vi.mocked(getGenericTasks).mock.calls[0];
    expect(params).toMatchObject({ workspaceId: 7 });
    expect(params.userIds).toBeUndefined();
  });

  it("lists the current user's tasks of all workspaces", async () => {
    setup();
    expect(await screen.findByText('eval-sweep')).toBeInTheDocument();
    const [params] = vi.mocked(getGenericTasks).mock.calls[0];
    expect(params.workspaceId).toBeUndefined();
    expect(params.userIds).toStrictEqual([CURRENT_USER_ID]);
  });

  describe('the row menu', () => {
    const row = async () => (await screen.findByText('eval-sweep')).closest('tr') as HTMLElement;

    it('is in the actions column, in the order of the task menu', async () => {
      setup(7);
      await userEvent.click(within(await row()).getByRole('button'));
      await screen.findByText('Copy Task ID');
      expect(menuLabels()).toEqual(FULL_MENU);
      expect(isDangerMenuItem('Kill')).toBe(true);
    });

    it('opens on a right click on the row too', async () => {
      setup(7);
      fireEvent.contextMenu(await row());
      await screen.findByText('Copy Task ID');
      expect(menuLabels()).toEqual(FULL_MENU);
    });

    it('kills the task after the confirmation and refreshes the list', async () => {
      setup(7);
      await userEvent.click(within(await row()).getByRole('button'));
      await userEvent.click(await screen.findByText('Kill'));
      expect(killGenericTask).not.toHaveBeenCalled();
      const fetches = vi.mocked(getGenericTasks).mock.calls.length;
      await userEvent.click(await screen.findByRole('button', { name: 'Kill' }));
      await waitFor(() =>
        expect(killGenericTask).toHaveBeenCalledWith({ killFromRoot: false, taskId: 'task-1' }),
      );
      await waitFor(() =>
        expect(vi.mocked(getGenericTasks).mock.calls.length).toBeGreaterThan(fetches),
      );
    });

    it('offers the retry of an unpause that failed from the other menu of the row', async () => {
      listed.state = GenericTaskState.Paused;
      vi.mocked(unpauseGenericTask).mockRejectedValueOnce(new Error('resume failed'));
      setup(7);
      await userEvent.click(within(await row()).getByRole('button'));
      await userEvent.click(await screen.findByText('Unpause'));
      // The failed unpause started the root, so the refreshed list reads it active.
      listed.state = GenericTaskState.Active;
      await userEvent.click(await screen.findByRole('button', { name: 'Unpause' }));
      await waitFor(() => expect(handleError).toHaveBeenCalled());
      await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
      await waitFor(async () =>
        expect(within(await row()).getByTestId('state')).toHaveTextContent(/active/i),
      );

      fireEvent.contextMenu(await row());
      await waitFor(() =>
        expect(menuLabels()).toEqual([
          'View Logs',
          'View Resources',
          'Copy Task ID',
          'Pause',
          'Retry Unpause',
          'Kill',
        ]),
      );
      await userEvent.click(openMenuItem('Retry Unpause'));
      const dialog = await screen.findByRole('dialog');
      expect(within(dialog).getByText(/^Retry the unpause of this task\?/)).toBeInTheDocument();
      await userEvent.click(within(dialog).getByRole('button', { name: 'Retry Unpause' }));
      await waitFor(() => expect(unpauseGenericTask).toHaveBeenCalledTimes(2));
    });

    it('disables Kill in the right-click menu while a kill from the column menu runs', async () => {
      let finishKill = () => {};
      vi.mocked(killGenericTask).mockImplementationOnce(
        () => new Promise<void>((resolve) => (finishKill = resolve)),
      );
      setup(7);
      await userEvent.click(within(await row()).getByRole('button'));
      await userEvent.click(await screen.findByText('Kill'));
      await userEvent.click(await screen.findByRole('button', { name: 'Kill' }));
      await waitFor(() => expect(killGenericTask).toHaveBeenCalled());

      fireEvent.contextMenu(await row());
      await waitFor(() => expect(isDisabledMenuItem('Kill')).toBe(true));
      expect(isDisabledMenuItem('Pause')).toBe(true);

      finishKill();
      await waitFor(() => expect(isDisabledMenuItem('Kill')).toBe(false));
    });
  });
});
