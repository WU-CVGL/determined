import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import { getGenericTasks, killGenericTask } from 'services/api';
import userStore from 'stores/users';
import { GenericTask, GenericTaskState } from 'types';
import { isDangerMenuItem, menuLabels } from 'utils/tests/menu';

import GenericTaskList from './GenericTaskList';

const CURRENT_USER_ID = 3;

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
    Promise.resolve({ pagination: { limit: 10, offset: 0, total: 1 }, tasks: [TASK] }),
  ),
  killGenericTask: vi.fn(() => Promise.resolve()),
  pauseGenericTask: vi.fn(() => Promise.resolve()),
  unpauseGenericTask: vi.fn(() => Promise.resolve()),
}));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canModifyWorkspaceNSC: () => true }),
}));
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => true }));

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
  });
});
