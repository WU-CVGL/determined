import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { MemoryRouter } from 'react-router-dom';

import GenericTaskActions from 'pages/GenericTaskDetails/GenericTaskActions';
import { killGenericTask, pauseGenericTask, unpauseGenericTask } from 'services/api';
import { GenericTask, GenericTaskState } from 'types';
import { copyToClipboard } from 'utils/dom';
import handleError from 'utils/error';
import { isDangerMenuItem, isDisabledMenuItem, menuLabels } from 'utils/tests/menu';

import GenericTaskActionDropdown from './GenericTaskActionDropdown';

const mocks = vi.hoisted(() => ({
  canModify: true,
  navigate: vi.fn(),
  resourcesEnabled: true as boolean | undefined,
}));

vi.mock('services/api', () => ({
  killGenericTask: vi.fn(() => Promise.resolve()),
  pauseGenericTask: vi.fn(() => Promise.resolve()),
  unpauseGenericTask: vi.fn(() => Promise.resolve()),
}));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({
    canModifyWorkspaceNSC: ({ userId }: { userId?: number }) => mocks.canModify && userId === 4,
  }),
}));
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => mocks.resourcesEnabled }));
vi.mock('utils/dom', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/dom')>()),
  copyToClipboard: vi.fn(() => Promise.resolve()),
}));
vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));
vi.mock('react-router-dom', async (importOriginal) => ({
  ...(await importOriginal<typeof import('react-router-dom')>()),
  useNavigate: () => mocks.navigate,
}));

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

const FULL_MENU = ['View Logs', 'View Resources', 'Copy Task ID', 'Pause', 'Kill'];

const renderMenu = (task: Partial<GenericTask> = {}, contextMenu = false) => {
  const onComplete = vi.fn();
  const props = { onComplete, task: { ...TASK, ...task } };
  render(
    <MemoryRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ConfirmationProvider>
          {contextMenu ? (
            <GenericTaskActionDropdown {...props}>
              <div>task row</div>
            </GenericTaskActionDropdown>
          ) : (
            <GenericTaskActionDropdown {...props} />
          )}
        </ConfirmationProvider>
      </UIProvider>
    </MemoryRouter>,
  );
  return { onComplete };
};

const openMenu = async (task: Partial<GenericTask> = {}, contextMenu = false) => {
  const rendered = renderMenu(task, contextMenu);
  if (contextMenu) fireEvent.contextMenu(screen.getByText('task row'));
  else await userEvent.click(screen.getByRole('button'));
  await screen.findByText('View Logs');
  return rendered;
};

/** The open confirmation: its title, its text and whether its OK button is red. */
const confirmation = async (okText: string) => {
  const dialog = await screen.findByRole('dialog');
  const ok = within(dialog).getByRole('button', { name: okText });
  return {
    danger: ok.classList.contains('ant-btn-dangerous'),
    ok,
    text: within(dialog).getByText(/\?/).textContent,
    title: dialog.querySelector('.ant-modal-title')?.textContent,
  };
};

describe('GenericTaskActionDropdown', () => {
  beforeEach(() => {
    mocks.canModify = true;
    mocks.resourcesEnabled = true;
  });

  afterEach(() => vi.clearAllMocks());

  describe('items', () => {
    it.each([
      ['action menu', false],
      ['right-click menu', true],
    ])(
      'lists every action of an active pausable task in the fixed order in the %s',
      async (_, contextMenu) => {
        await openMenu({}, contextMenu);
        expect(menuLabels()).toEqual(FULL_MENU);
      },
    );

    it('shows Kill in red and the other actions not', async () => {
      await openMenu();
      expect(isDangerMenuItem('Kill')).toBe(true);
      FULL_MENU.filter((label) => label !== 'Kill').forEach((label) =>
        expect(isDangerMenuItem(label)).toBe(false),
      );
    });

    it('offers Unpause instead of Pause for a paused task', async () => {
      await openMenu({ state: GenericTaskState.Paused });
      expect(menuLabels()).toEqual([
        'View Logs',
        'View Resources',
        'Copy Task ID',
        'Unpause',
        'Kill',
      ]);
    });

    it('leaves out Pause for a task that was not created pausable', async () => {
      await openMenu({ noPause: true });
      expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Copy Task ID', 'Kill']);
    });

    it('offers only Kill of the controls while a task is pausing', async () => {
      await openMenu({ state: GenericTaskState.StoppingPaused });
      expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Copy Task ID', 'Kill']);
    });

    it.each([GenericTaskState.Completed, GenericTaskState.Canceled, GenericTaskState.Error])(
      'offers no controls for an ended task (%s)',
      async (state) => {
        await openMenu({ state });
        expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Copy Task ID']);
      },
    );

    it('offers no controls without permission to control the task', async () => {
      mocks.canModify = false;
      await openMenu();
      expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Copy Task ID']);
    });

    it('offers no controls on another user’s task to a user who is not an admin', async () => {
      await openMenu({ userId: 5 });
      expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Copy Task ID']);
    });

    it.each([false, undefined])(
      'leaves out View Resources when the master does not offer it (%s)',
      async (enabled) => {
        mocks.resourcesEnabled = enabled;
        await openMenu();
        expect(menuLabels()).toEqual(FULL_MENU.filter((label) => label !== 'View Resources'));
      },
    );
  });

  describe('viewing', () => {
    it.each([
      ['View Logs', '/generic-tasks/task-1/logs'],
      ['View Resources', '/generic-tasks/task-1/resources'],
    ])('%s opens that tab of the task’s page', async (label, path) => {
      await openMenu();
      await userEvent.click(screen.getByText(label));
      expect(mocks.navigate).toHaveBeenCalledWith(path);
    });

    it('copies the task ID', async () => {
      await openMenu();
      await userEvent.click(screen.getByText('Copy Task ID'));
      expect(copyToClipboard).toHaveBeenCalledWith('task-1');
      expect(await screen.findByText('Task ID has been copied to clipboard.')).toBeInTheDocument();
    });
  });

  describe('controls', () => {
    it('kills the task and its descendants only after a red confirmation', async () => {
      const { onComplete } = await openMenu();
      await userEvent.click(screen.getByText('Kill'));
      const dialog = await confirmation('Kill');
      expect(dialog.title).toBe('Kill eval-sweep');
      expect(dialog.text).toBe('Kill this task and all its descendants?');
      expect(dialog.danger).toBe(true);
      expect(killGenericTask).not.toHaveBeenCalled();

      await userEvent.click(dialog.ok);
      await waitFor(() =>
        expect(killGenericTask).toHaveBeenCalledWith({ killFromRoot: false, taskId: 'task-1' }),
      );
      await waitFor(() => expect(onComplete).toHaveBeenCalled());
    });

    it('pauses only after a confirmation that is not red', async () => {
      const { onComplete } = await openMenu();
      await userEvent.click(screen.getByText('Pause'));
      const dialog = await confirmation('Pause');
      expect(dialog.danger).toBe(false);
      expect(pauseGenericTask).not.toHaveBeenCalled();

      await userEvent.click(dialog.ok);
      await waitFor(() => expect(pauseGenericTask).toHaveBeenCalledWith({ taskId: 'task-1' }));
      await waitFor(() => expect(onComplete).toHaveBeenCalled());
    });

    it('unpauses only after a confirmation', async () => {
      await openMenu({ state: GenericTaskState.Paused });
      await userEvent.click(screen.getByText('Unpause'));
      const dialog = await confirmation('Unpause');
      expect(unpauseGenericTask).not.toHaveBeenCalled();

      await userEvent.click(dialog.ok);
      await waitFor(() => expect(unpauseGenericTask).toHaveBeenCalledWith({ taskId: 'task-1' }));
    });

    it('does nothing when the confirmation is cancelled', async () => {
      await openMenu();
      await userEvent.click(screen.getByText('Kill'));
      await confirmation('Kill');
      await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(killGenericTask).not.toHaveBeenCalled();
    });

    it('works from the right-click menu', async () => {
      await openMenu({}, true);
      await userEvent.click(screen.getByText('Kill'));
      await userEvent.click((await confirmation('Kill')).ok);
      await waitFor(() => expect(killGenericTask).toHaveBeenCalledTimes(1));
    });

    it('offers a retry with the retry confirmation after an unpause failed', async () => {
      vi.mocked(unpauseGenericTask).mockRejectedValueOnce(new Error('resume failed'));
      await openMenu({ state: GenericTaskState.Paused });
      await userEvent.click(screen.getByText('Unpause'));
      await userEvent.click((await confirmation('Unpause')).ok);
      await waitFor(() => expect(handleError).toHaveBeenCalled());
      // The confirmation stays open after a failure; close it.
      await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

      await userEvent.click(screen.getByRole('button'));
      await waitFor(() =>
        expect(menuLabels()).toEqual([
          'View Logs',
          'View Resources',
          'Copy Task ID',
          'Retry Unpause',
          'Kill',
        ]),
      );
      await userEvent.click(screen.getByText('Retry Unpause'));
      const dialog = await confirmation('Retry Unpause');
      expect(dialog.title).toBe('Retry unpause of eval-sweep');
      expect(dialog.text).toBe(
        'Retry the unpause of this task? The master resumes the members of its tree that the ' +
          'failed unpause did not start, or refuses if there is nothing left to resume.',
      );
      await userEvent.click(dialog.ok);
      await waitFor(() => expect(unpauseGenericTask).toHaveBeenCalledTimes(2));
      expect(vi.mocked(unpauseGenericTask).mock.calls[1][0]).toStrictEqual({ taskId: 'task-1' });
    });

    it('disables Pause and Kill while an action on the task runs', async () => {
      let finishKill = () => {};
      vi.mocked(killGenericTask).mockImplementationOnce(
        () => new Promise<void>((resolve) => (finishKill = resolve)),
      );
      await openMenu({}, true);
      await userEvent.click(screen.getByText('Kill'));
      await userEvent.click((await confirmation('Kill')).ok);
      await waitFor(() => expect(killGenericTask).toHaveBeenCalled());

      fireEvent.contextMenu(screen.getByText('task row'));
      await waitFor(() => expect(isDisabledMenuItem('Kill')).toBe(true));
      expect(isDisabledMenuItem('Pause')).toBe(true);
      expect(isDisabledMenuItem('Copy Task ID')).toBe(false);

      finishKill();
      await waitFor(() => expect(isDisabledMenuItem('Kill')).toBe(false));
      expect(isDisabledMenuItem('Pause')).toBe(false);
    });
  });

  describe('the confirmations of the task’s page', () => {
    /** The confirmation that the task page's button opens. */
    const pageConfirmation = async (
      task: Partial<GenericTask>,
      button: string,
      okText = button,
    ) => {
      const view = render(
        <UIProvider theme={DefaultTheme.Light}>
          <ConfirmationProvider>
            <GenericTaskActions canControl task={{ ...TASK, ...task }} />
          </ConfirmationProvider>
        </UIProvider>,
      );
      await userEvent.click(screen.getByRole('button', { name: button }));
      const { danger, text, title } = await confirmation(okText);
      view.unmount();
      return { danger, text, title };
    };

    /** The confirmation that the menu's item opens. */
    const menuConfirmation = async (task: Partial<GenericTask>, item: string) => {
      await openMenu(task);
      await userEvent.click(screen.getByText(item));
      const { danger, text, title } = await confirmation(item);
      await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      return { danger, text, title };
    };

    it.each([
      ['Kill', {}],
      ['Pause', {}],
      ['Unpause', { state: GenericTaskState.Paused }],
    ] as const)('are the menu’s for %s', async (action, task) => {
      const page = await pageConfirmation(task, action);
      const menu = await menuConfirmation(task, action);
      expect(menu).toEqual(page);
    });
  });
});
