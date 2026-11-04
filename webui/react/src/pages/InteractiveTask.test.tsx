import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import {
  getCommand,
  getShells,
  getTask,
  getTaskServices,
  getTensorBoard,
  killTask,
} from 'services/api';
import {
  TaskServiceProtocol,
  V1GetTaskServicesResponse,
  V1TaskServiceState,
  V1TaskType,
} from 'services/api-ts-sdk';
import { CommandState, CommandTask, CommandType, TaskItem } from 'types';
import { DetError } from 'utils/error';
import { TaskPhase } from 'utils/task';

import InteractiveTask from './InteractiveTask';

const TASK_ID = 'task-1';

// No getShell and no getTaskServiceCredential: the page must never read a credential when it
// loads. Vitest throws if a module uses an export that the mock does not define.
vi.mock('services/api', () => ({
  getCommand: vi.fn(),
  getShells: vi.fn(),
  getTask: vi.fn(),
  getTaskServices: vi.fn(),
  getTensorBoard: vi.fn(),
  killTask: vi.fn(),
}));

vi.mock('components/ShellTerminalPane', () => ({
  default: ({ active, phase, shellId }: { active: boolean; phase: TaskPhase; shellId: string }) => (
    <div
      data-active={String(active)}
      data-phase={phase}
      data-shell={shellId}
      data-testid="terminal-pane"
    />
  ),
}));

vi.mock('components/TaskServicePane', () => ({
  default: ({ phase, service }: { phase: TaskPhase; service: string }) => (
    <div data-phase={phase} data-testid={`service-pane-${service}`} />
  ),
}));

vi.mock('pages/TaskLogs', () => ({ TaskLogsViewer: () => <div data-testid="logs-pane" /> }));
vi.mock('components/TaskResourcesPanel', () => ({
  default: () => <div data-testid="resources-pane" />,
}));
vi.mock('hooks/useSettings', () => ({
  useSettings: () => ({ resetSettings: vi.fn(), settings: {}, updateSettings: vi.fn() }),
}));
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => true }));
const feature = vi.hoisted(() => ({ on: true }));
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => feature.on }) }));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canModifyWorkspaceNSC: () => true }),
}));
vi.mock('utils/routes', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/routes')>()),
  routeToReactUrl: vi.fn(),
}));

const shell: CommandTask = {
  id: TASK_ID,
  name: 'Shell (brave-otter)',
  resourcePool: 'default',
  startTime: '2026-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.Shell,
  userId: 7,
  workspaceId: 1,
};

const taskWith = (
  isReady: boolean,
  taskType: V1TaskType = V1TaskType.SHELL,
  state: CommandState = CommandState.Running,
  endTime?: string,
): TaskItem => ({
  allocations: [{ isReady, state, taskId: TASK_ID }],
  endTime,
  startTime: '2026-01-01T00:00:00Z',
  taskId: TASK_ID,
  taskType,
});

const servicesWith = (names: string[], mayConnect = true): V1GetTaskServicesResponse => ({
  primary: names[0] ?? '',
  services: names.map((name) => ({
    mayConnect,
    name,
    protocol: name === 'ssh' ? TaskServiceProtocol.TCP : TaskServiceProtocol.HTTP,
    serviceId: name === 'ssh' ? TASK_ID : `${TASK_ID}:2900`,
    state: V1TaskServiceState.STARTING,
    url: name === 'ssh' ? '' : `/proxy/${TASK_ID}:2900/`,
  })),
  taskId: TASK_ID,
  userId: 7,
  workspaceId: 1,
});

const setup = (search = '') =>
  render(
    <MemoryRouter initialEntries={[`/tasks/${TASK_ID}/interactive${search}`]}>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <HelmetProvider>
            <ConfirmationProvider>
              <Routes>
                <Route element={<InteractiveTask />} path="/tasks/:taskId/interactive" />
              </Routes>
            </ConfirmationProvider>
          </HelmetProvider>
        </ThemeProvider>
      </UIProvider>
    </MemoryRouter>,
  );

const tabLabels = () =>
  Array.from(document.querySelectorAll('.ant-tabs-tab-btn')).map((tab) => tab.textContent);

describe('InteractiveTask', () => {
  beforeEach(() => {
    feature.on = true;
    vi.mocked(getTask).mockReset().mockResolvedValue(taskWith(false));
    vi.mocked(getTaskServices)
      .mockReset()
      .mockResolvedValue(servicesWith(['ssh', 'jupyter', 'vscode']));
    vi.mocked(getShells).mockReset().mockResolvedValue([shell]);
    vi.mocked(getCommand).mockReset();
    vi.mocked(getTensorBoard).mockReset();
    vi.mocked(killTask).mockReset();
  });

  it('shows the services of a shell as tabs, with its name and pool from the shell list', async () => {
    setup();
    expect(await screen.findByText(shell.name)).toBeInTheDocument();
    expect(screen.getByText(shell.resourcePool)).toBeInTheDocument();
    expect(tabLabels()).toEqual(['Terminal', 'JupyterLab', 'VS Code', 'Logs', 'Resources']);
    expect(document.title).toBe(shell.name);
    expect(getShells).toHaveBeenCalledWith({});
    // The first tab is shown; the others are not mounted until shown.
    const terminal = screen.getByTestId('terminal-pane');
    expect(terminal).toHaveAttribute('data-active', 'true');
    expect(terminal).toHaveAttribute('data-phase', 'waiting');
    expect(terminal).toHaveAttribute('data-shell', TASK_ID);
    expect(screen.queryByTestId('service-pane-jupyter')).not.toBeInTheDocument();
    expect(screen.queryByTestId('logs-pane')).not.toBeInTheDocument();
  });

  it('feeds the panes the phase of the task from one poll, which stops once the task ends', async () => {
    vi.useFakeTimers();
    try {
      setup();
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-phase', 'waiting');
      expect(getTask).toHaveBeenCalledTimes(1);

      vi.mocked(getTask).mockResolvedValue(taskWith(true));
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-phase', 'ready');

      vi.mocked(getTask).mockResolvedValue(
        taskWith(false, V1TaskType.SHELL, CommandState.Terminated, '2026-01-01T01:00:00Z'),
      );
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-phase', 'ended');
      expect(getTask).toHaveBeenCalledTimes(3);
      await act(() => vi.advanceTimersByTimeAsync(6000));
      expect(getTask).toHaveBeenCalledTimes(3);
    } finally {
      vi.useRealTimers();
    }
  });

  it('mounts a pane when its tab is first shown and keeps it mounted after', async () => {
    setup();
    await screen.findByTestId('terminal-pane');
    await userEvent.click(screen.getByRole('tab', { name: 'JupyterLab' }));
    expect(await screen.findByTestId('service-pane-jupyter')).toBeInTheDocument();
    expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-active', 'false');
    expect(screen.queryByTestId('service-pane-vscode')).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole('tab', { name: 'Terminal' }));
    await waitFor(() =>
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-active', 'true'),
    );
    expect(screen.getByTestId('service-pane-jupyter')).toBeInTheDocument();
  });

  it('lands on the tab of the query, or the first tab', async () => {
    setup('?tab=logs');
    expect(await screen.findByTestId('logs-pane')).toBeInTheDocument();
    expect(screen.queryByTestId('terminal-pane')).not.toBeInTheDocument();
  });

  it('leaves out the Terminal tab when the user may not connect or the master turns it off', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(servicesWith(['ssh', 'jupyter'], false));
    const { unmount } = setup();
    expect(await screen.findByTestId('service-pane-jupyter')).toBeInTheDocument();
    expect(tabLabels()).toEqual(['JupyterLab', 'Logs', 'Resources']);
    unmount();

    feature.on = false;
    vi.mocked(getTaskServices).mockResolvedValue(servicesWith(['ssh', 'jupyter']));
    setup();
    expect(await screen.findByTestId('service-pane-jupyter')).toBeInTheDocument();
    expect(tabLabels()).toEqual(['JupyterLab', 'Logs', 'Resources']);
  });

  it('shows a TensorBoard in a frame of its wait page', async () => {
    vi.mocked(getTask).mockResolvedValue(taskWith(false, V1TaskType.TENSORBOARD));
    vi.mocked(getTaskServices).mockResolvedValue(servicesWith([]));
    vi.mocked(getTensorBoard).mockResolvedValue({
      ...shell,
      name: 'TensorBoard (lively-fox)',
      serviceAddress: `/proxy/${TASK_ID}/`,
      type: CommandType.TensorBoard,
    });
    setup();
    const frame = await screen.findByTitle('TensorBoard');
    expect(frame).toHaveAttribute(
      'src',
      expect.stringContaining(`/wait/tensor-board/${TASK_ID}?serviceAddr=/proxy/${TASK_ID}/`),
    );
    expect(tabLabels()).toEqual(['TensorBoard', 'Logs', 'Resources']);
    expect(screen.getByText('TensorBoard (lively-fox)')).toBeInTheDocument();
    expect(getShells).not.toHaveBeenCalled();
  });

  it('gives a command its logs and resources', async () => {
    vi.mocked(getTask).mockResolvedValue(taskWith(false, V1TaskType.COMMAND));
    vi.mocked(getTaskServices).mockResolvedValue(servicesWith([]));
    vi.mocked(getCommand).mockResolvedValue({
      ...shell,
      name: 'Command',
      type: CommandType.Command,
    });
    setup();
    expect(await screen.findByTestId('logs-pane')).toBeInTheDocument();
    expect(tabLabels()).toEqual(['Logs', 'Resources']);
    expect(await screen.findByText('Command')).toBeInTheDocument();
  });

  it('names an ended task by its ID when its lookup is gone', async () => {
    vi.mocked(getTask).mockResolvedValue(
      taskWith(false, V1TaskType.SHELL, CommandState.Terminated, '2026-01-01T01:00:00Z'),
    );
    vi.mocked(getShells).mockResolvedValue([]);
    setup();
    expect(await screen.findByTestId('terminal-pane')).toHaveAttribute('data-phase', 'ended');
    expect(screen.getByText(TASK_ID)).toBeInTheDocument();
    expect(document.title).toBe(TASK_ID);
  });

  it('shows why the task cannot be shown', async () => {
    vi.mocked(getTask).mockResolvedValue(undefined);
    setup();
    expect(await screen.findByText(`Task ${TASK_ID} not found.`)).toBeInTheDocument();
    expect(screen.queryByRole('tab')).not.toBeInTheDocument();
  });

  it('gives up on the task after repeated failures, with the error', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTask).mockImplementation(() =>
        Promise.reject(new DetError(undefined, { publicMessage: 'task not found' })),
      );
      setup();
      // Four failures: at the start and after 2, 4 and 6 seconds.
      await act(() => vi.advanceTimersByTimeAsync(7000));
      expect(getTask).toHaveBeenCalledTimes(4);
      expect(screen.queryByText('task not found')).not.toBeInTheDocument();
      await act(() => vi.advanceTimersByTimeAsync(1000));
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByText('task not found')).toBeInTheDocument();
      expect(getTask).toHaveBeenCalledTimes(5);
      await act(() => vi.advanceTimersByTimeAsync(4000));
      expect(getTask).toHaveBeenCalledTimes(5);
    } finally {
      vi.useRealTimers();
    }
  });

  it('switches to Logs from the task bar, and kills the task from it', async () => {
    setup();
    await screen.findByTestId('terminal-pane');
    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await userEvent.click(await screen.findByText('View Logs'));
    expect(await screen.findByTestId('logs-pane')).toBeInTheDocument();
    expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-active', 'false');

    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await userEvent.click(await screen.findByText('Kill'));
    await userEvent.click(await screen.findByRole('button', { name: 'Kill' }));
    await waitFor(() =>
      expect(killTask).toHaveBeenCalledWith({ id: TASK_ID, type: CommandType.Shell }),
    );
  });
});
