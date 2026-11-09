import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { getCommand, getShells, getTask, getTaskServices, killTask } from 'services/api';
import {
  TaskServiceProtocol,
  V1GetTaskServicesResponse,
  V1TaskServiceState,
  V1TaskType,
} from 'services/api-ts-sdk';
import { CommandState, CommandTask, CommandType, TaskItem } from 'types';
import { DetError } from 'utils/error';
import { TaskPhase } from 'utils/task';
import { isDisabledMenuItem } from 'utils/tests/menu';

import InteractiveTask from './InteractiveTask';

const TASK_ID = 'task-1';

// No getShell and no getTaskServiceCredential: the page must never read a credential when it
// loads. Vitest throws if a module uses an export that the mock does not define.
vi.mock('services/api', () => ({
  getCommand: vi.fn(),
  getShells: vi.fn(),
  getTask: vi.fn(),
  getTaskServices: vi.fn(),
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
  default: ({
    ended,
    service,
    state,
  }: {
    ended: boolean;
    service: string;
    state: V1TaskServiceState;
  }) => (
    <div data-ended={String(ended)} data-state={state} data-testid={`service-pane-${service}`} />
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

const servicesWith = (
  names: string[],
  mayConnect = true,
  state: V1TaskServiceState = V1TaskServiceState.STARTING,
): V1GetTaskServicesResponse => ({
  primary: names[0] ?? '',
  services: names.map((name) => ({
    mayConnect,
    name,
    protocol: name === 'ssh' ? TaskServiceProtocol.TCP : TaskServiceProtocol.HTTP,
    serviceId: name === 'ssh' ? TASK_ID : `${TASK_ID}:2900`,
    state,
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
      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'jupyter'], true, V1TaskServiceState.READY),
      );
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

  it('reads the task one tick at a time, so that an older answer never follows a newer one', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'jupyter'], true, V1TaskServiceState.READY),
      );
      let answer: (task: TaskItem) => void = () => undefined;
      vi.mocked(getTask).mockResolvedValueOnce(taskWith(true));
      vi.mocked(getTask).mockImplementationOnce(
        () => new Promise<TaskItem>((resolve) => (answer = resolve)),
      );
      setup();
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-phase', 'ready');
      expect(getTask).toHaveBeenCalledTimes(2);
      // The second read has not answered: no third one starts, however long it takes.
      await act(() => vi.advanceTimersByTimeAsync(6000));
      expect(getTask).toHaveBeenCalledTimes(2);

      act(() =>
        answer(taskWith(false, V1TaskType.SHELL, CommandState.Terminated, '2026-01-01T01:00:00Z')),
      );
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-phase', 'ended');
      // Ended: the poll stops, and nothing brings the page back to ready.
      await act(() => vi.advanceTimersByTimeAsync(6000));
      expect(getTask).toHaveBeenCalledTimes(2);
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-phase', 'ended');
    } finally {
      vi.useRealTimers();
    }
  });

  it('feeds a pane the state of its service, read again while a service has not settled', async () => {
    vi.useFakeTimers();
    try {
      setup('?tab=jupyter');
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByTestId('service-pane-jupyter')).toHaveAttribute(
        'data-state',
        V1TaskServiceState.STARTING,
      );
      expect(getTaskServices).toHaveBeenCalledTimes(1);

      // A failed read keeps the last services.
      vi.mocked(getTaskServices).mockRejectedValueOnce(new Error('network'));
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(tabLabels()).toEqual(['Terminal', 'JupyterLab', 'VS Code', 'Logs', 'Resources']);
      expect(getTaskServices).toHaveBeenCalledTimes(2);

      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'jupyter', 'vscode'], true, V1TaskServiceState.READY),
      );
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(screen.getByTestId('service-pane-jupyter')).toHaveAttribute(
        'data-state',
        V1TaskServiceState.READY,
      );
      expect(getTaskServices).toHaveBeenCalledTimes(3);
      // Settled: the task is still polled every 2 s, the services every 10 s.
      await act(() => vi.advanceTimersByTimeAsync(8000));
      expect(getTaskServices).toHaveBeenCalledTimes(3);
      expect(getTask).toHaveBeenCalledTimes(7);
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(getTaskServices).toHaveBeenCalledTimes(4);
    } finally {
      vi.useRealTimers();
    }
  });

  it('shows the state of a service that exits after it was ready, from the slow poll', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'jupyter', 'tensorboard'], true, V1TaskServiceState.READY),
      );
      setup('?tab=tensorboard');
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(tabLabels()).toEqual(['Terminal', 'JupyterLab', 'TensorBoard', 'Logs', 'Resources']);
      expect(screen.getByTestId('service-pane-tensorboard')).toHaveAttribute(
        'data-state',
        V1TaskServiceState.READY,
      );
      expect(getTaskServices).toHaveBeenCalledTimes(1);

      const exited = servicesWith(
        ['ssh', 'jupyter', 'tensorboard'],
        true,
        V1TaskServiceState.READY,
      );
      exited.services[2].state = V1TaskServiceState.EXITED;
      vi.mocked(getTaskServices).mockResolvedValue(exited);
      await act(() => vi.advanceTimersByTimeAsync(8000));
      expect(getTaskServices).toHaveBeenCalledTimes(1);
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(getTaskServices).toHaveBeenCalledTimes(2);
      // The tab stays, with the service's state.
      expect(tabLabels()).toEqual(['Terminal', 'JupyterLab', 'TensorBoard', 'Logs', 'Resources']);
      expect(screen.getByTestId('service-pane-tensorboard')).toHaveAttribute(
        'data-state',
        V1TaskServiceState.EXITED,
      );
    } finally {
      vi.useRealTimers();
    }
  });

  it('reads no services while the page is hidden, and reads them at once when it is shown', async () => {
    let hidden = false;
    const visibility = vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
    vi.useFakeTimers();
    try {
      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'jupyter'], true, V1TaskServiceState.READY),
      );
      setup();
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getTaskServices).toHaveBeenCalledTimes(1);

      hidden = true;
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
      await act(() => vi.advanceTimersByTimeAsync(20000));
      expect(getTaskServices).toHaveBeenCalledTimes(1);
      // The task itself is still polled.
      expect(getTask).toHaveBeenCalledTimes(11);

      hidden = false;
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getTaskServices).toHaveBeenCalledTimes(2);
      // The slow poll goes on from that read.
      await act(() => vi.advanceTimersByTimeAsync(8000));
      expect(getTaskServices).toHaveBeenCalledTimes(2);
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(getTaskServices).toHaveBeenCalledTimes(3);
    } finally {
      vi.useRealTimers();
      visibility.mockRestore();
    }
  });

  it('reads the services once more when the task ends, then stops', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'jupyter'], true, V1TaskServiceState.READY),
      );
      setup('?tab=jupyter');
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getTaskServices).toHaveBeenCalledTimes(1);

      vi.mocked(getTask).mockResolvedValue(
        taskWith(false, V1TaskType.SHELL, CommandState.Terminated, '2026-01-01T01:00:00Z'),
      );
      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'jupyter'], true, V1TaskServiceState.EXITED),
      );
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(getTaskServices).toHaveBeenCalledTimes(2);
      expect(screen.getByTestId('service-pane-jupyter')).toHaveAttribute(
        'data-state',
        V1TaskServiceState.EXITED,
      );
      await act(() => vi.advanceTimersByTimeAsync(20000));
      expect(getTaskServices).toHaveBeenCalledTimes(2);
      expect(getTask).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  // Service READY -> task ends -> the last services read fails -> the page must not keep showing a
  // live service; showing the page again must not sit in a silent stale state.
  it('shows the end and the failure of the last read of the services, not a live service', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'tensorboard'], true, V1TaskServiceState.READY),
      );
      setup('?tab=tensorboard');
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByTestId('service-pane-tensorboard')).toHaveAttribute('data-ended', 'false');
      expect(getTaskServices).toHaveBeenCalledTimes(1);

      vi.mocked(getTask).mockResolvedValue(
        taskWith(false, V1TaskType.SHELL, CommandState.Terminated, '2026-01-01T01:00:00Z'),
      );
      vi.mocked(getTaskServices).mockRejectedValue(
        new DetError(undefined, { publicMessage: 'master unreachable' }),
      );
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(getTaskServices).toHaveBeenCalledTimes(2);
      expect(screen.getByTestId('service-pane-tensorboard')).toHaveAttribute('data-ended', 'true');
      expect(screen.getByText('master unreachable')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();

      // Shown again: the end and the failure stay, with Retry.
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
      await act(() => vi.advanceTimersByTimeAsync(20000));
      expect(getTaskServices).toHaveBeenCalledTimes(2);
      expect(screen.getByTestId('service-pane-tensorboard')).toHaveAttribute('data-ended', 'true');
      expect(screen.getByText('master unreachable')).toBeInTheDocument();

      // A failed Retry shows its own failure; a successful one the final states.
      vi.mocked(getTaskServices).mockRejectedValueOnce(
        new DetError(undefined, { publicMessage: 'still unreachable' }),
      );
      fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getTaskServices).toHaveBeenCalledTimes(3);
      expect(screen.getByText('still unreachable')).toBeInTheDocument();

      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'tensorboard'], true, V1TaskServiceState.EXITED),
      );
      fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getTaskServices).toHaveBeenCalledTimes(4);
      expect(screen.queryByText('still unreachable')).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
      expect(screen.getByTestId('service-pane-tensorboard')).toHaveAttribute(
        'data-state',
        V1TaskServiceState.EXITED,
      );
      await act(() => vi.advanceTimersByTimeAsync(20000));
      expect(getTaskServices).toHaveBeenCalledTimes(4);
    } finally {
      vi.useRealTimers();
    }
  });

  it('reads the services after the end, past a read the shown page started before it', async () => {
    vi.useFakeTimers();
    try {
      const ready = servicesWith(['ssh', 'jupyter'], true, V1TaskServiceState.READY);
      vi.mocked(getTaskServices).mockResolvedValue(ready);
      setup('?tab=jupyter');
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getTaskServices).toHaveBeenCalledTimes(1);

      // The page is shown again: its read is still in flight when the poll sees the end.
      let answer: (value: V1GetTaskServicesResponse) => void = () => undefined;
      vi.mocked(getTaskServices).mockReturnValueOnce(
        new Promise((resolve) => {
          answer = resolve;
        }),
      );
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
      expect(getTaskServices).toHaveBeenCalledTimes(2);
      vi.mocked(getTask).mockResolvedValue(
        taskWith(false, V1TaskType.SHELL, CommandState.Terminated, '2026-01-01T01:00:00Z'),
      );
      vi.mocked(getTaskServices).mockResolvedValue(
        servicesWith(['ssh', 'jupyter'], true, V1TaskServiceState.EXITED),
      );
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(getTaskServices).toHaveBeenCalledTimes(2);

      await act(async () => {
        answer(ready);
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(getTaskServices).toHaveBeenCalledTimes(3);
      expect(screen.getByTestId('service-pane-jupyter')).toHaveAttribute(
        'data-state',
        V1TaskServiceState.EXITED,
      );
      await act(() => vi.advanceTimersByTimeAsync(20000));
      expect(getTaskServices).toHaveBeenCalledTimes(3);
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

  it('leaves out the service tabs when the user may not connect, with no credential read', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(servicesWith(['ssh', 'jupyter'], false));
    setup('?tab=terminal');
    expect(await screen.findByTestId('logs-pane')).toBeInTheDocument();
    expect(tabLabels()).toEqual(['Logs', 'Resources']);
    expect(screen.queryByTestId('terminal-pane')).not.toBeInTheDocument();
    expect(screen.queryByTestId('service-pane-jupyter')).not.toBeInTheDocument();
  });

  it('leaves out the Terminal tab when the master turns it off', async () => {
    feature.on = false;
    vi.mocked(getTaskServices).mockResolvedValue(servicesWith(['ssh', 'jupyter']));
    setup();
    expect(await screen.findByTestId('service-pane-jupyter')).toBeInTheDocument();
    expect(tabLabels()).toEqual(['JupyterLab', 'Logs', 'Resources']);
  });

  it('shows a shell’s tensorboard service as its TensorBoard tab, after VS Code', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(
      servicesWith(['ssh', 'jupyter', 'tensorboard', 'vscode']),
    );
    setup('?tab=tensorboard');
    const pane = await screen.findByTestId('service-pane-tensorboard');
    expect(pane).toHaveAttribute('data-state', V1TaskServiceState.STARTING);
    expect(tabLabels()).toEqual([
      'Terminal',
      'JupyterLab',
      'VS Code',
      'TensorBoard',
      'Logs',
      'Resources',
    ]);
  });

  it('shows a TensorBoard session, a shell without ssh, as its TensorBoard tab without a Terminal', async () => {
    vi.mocked(getShells).mockResolvedValue([{ ...shell, name: 'TensorBoard (brave-cat)' }]);
    vi.mocked(getTaskServices).mockResolvedValue(
      servicesWith(['tensorboard'], true, V1TaskServiceState.READY),
    );
    setup('?tab=tensorboard');
    const pane = await screen.findByTestId('service-pane-tensorboard');
    expect(pane).toHaveAttribute('data-state', V1TaskServiceState.READY);
    expect(tabLabels()).toEqual(['TensorBoard', 'Logs', 'Resources']);
    expect(screen.queryByTestId('terminal-pane')).not.toBeInTheDocument();
  });

  it('leaves out the TensorBoard tab when the user may not connect to the service', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(servicesWith(['ssh', 'tensorboard'], false));
    setup();
    expect(await screen.findByTestId('logs-pane')).toBeInTheDocument();
    expect(tabLabels()).toEqual(['Logs', 'Resources']);
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

  it('gives a task of another type, which has no lookup and no services, its logs and resources', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTask).mockResolvedValue(taskWith(false, V1TaskType.GENERIC));
      vi.mocked(getTaskServices).mockRejectedValue(
        new DetError(undefined, { publicMessage: `task ${TASK_ID} not found` }),
      );
      setup();
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByTestId('logs-pane')).toBeInTheDocument();
      expect(tabLabels()).toEqual(['Logs', 'Resources']);
      expect(screen.getByText(TASK_ID)).toBeInTheDocument();
      expect(getShells).not.toHaveBeenCalled();
      expect(getCommand).not.toHaveBeenCalled();
      // Not asked again: the services API does not know the task.
      await act(() => vi.advanceTimersByTimeAsync(20000));
      expect(getTaskServices).toHaveBeenCalledTimes(1);
      expect(getTask).toHaveBeenCalledTimes(11);
    } finally {
      vi.useRealTimers();
    }
  });

  it('shows the tabs once a failed read of the services succeeds', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTaskServices).mockRejectedValueOnce(new Error('network'));
      setup();
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.queryByRole('tab')).not.toBeInTheDocument();
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(tabLabels()).toEqual(['Terminal', 'JupyterLab', 'VS Code', 'Logs', 'Resources']);
      expect(getTaskServices).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  // With no tab in the query, as the task links open the page, and with the Logs tab in it.
  it.each(['', '?tab=logs'])(
    'shows the logs and resources with Retry when the services cannot be read, and the service tabs on the same tab once they are (query %j)',
    async (search) => {
      vi.useFakeTimers();
      try {
        vi.mocked(getTaskServices).mockImplementation(() =>
          Promise.reject(new DetError(undefined, { publicMessage: 'master unreachable' })),
        );
        setup(search);
        // Four failures: at the start and after 2, 4 and 6 seconds.
        await act(() => vi.advanceTimersByTimeAsync(7000));
        expect(getTaskServices).toHaveBeenCalledTimes(4);
        expect(screen.queryByRole('tab')).not.toBeInTheDocument();
        await act(() => vi.advanceTimersByTimeAsync(1000));
        await act(() => vi.advanceTimersByTimeAsync(0));
        expect(getTaskServices).toHaveBeenCalledTimes(5);
        expect(screen.getByText('master unreachable')).toBeInTheDocument();
        expect(tabLabels()).toEqual(['Logs', 'Resources']);
        expect(screen.getByTestId('logs-pane')).toBeInTheDocument();
        // The task is still polled; the services are not until Retry.
        await act(() => vi.advanceTimersByTimeAsync(4000));
        expect(getTaskServices).toHaveBeenCalledTimes(5);
        expect(getTask).toHaveBeenCalledTimes(7);

        // One read per Retry; a failure is shown again.
        vi.mocked(getTaskServices).mockRejectedValueOnce(
          new DetError(undefined, { publicMessage: 'still unreachable' }),
        );
        fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
        await act(() => vi.advanceTimersByTimeAsync(0));
        expect(getTaskServices).toHaveBeenCalledTimes(6);
        expect(screen.getByText('still unreachable')).toBeInTheDocument();
        expect(tabLabels()).toEqual(['Logs', 'Resources']);

        vi.mocked(getTaskServices).mockResolvedValue(
          servicesWith(['ssh', 'jupyter', 'vscode'], true, V1TaskServiceState.READY),
        );
        fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
        await act(() => vi.advanceTimersByTimeAsync(0));
        expect(getTaskServices).toHaveBeenCalledTimes(7);
        expect(screen.queryByText('still unreachable')).not.toBeInTheDocument();
        expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
        expect(tabLabels()).toEqual(['Terminal', 'JupyterLab', 'VS Code', 'Logs', 'Resources']);
        // The logs stay mounted and shown; no other pane is mounted.
        expect(document.querySelector('.ant-tabs-tab-active')?.textContent).toBe('Logs');
        expect(screen.getByTestId('logs-pane')).toBeInTheDocument();
        expect(screen.queryByTestId('terminal-pane')).not.toBeInTheDocument();
      } finally {
        vi.useRealTimers();
      }
    },
  );

  it('takes a not-found answer to Retry as final: no services, and no notice', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTaskServices).mockImplementation(() =>
        Promise.reject(new DetError(undefined, { publicMessage: 'master unreachable' })),
      );
      setup();
      await act(() => vi.advanceTimersByTimeAsync(8000));
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByText('master unreachable')).toBeInTheDocument();

      vi.mocked(getTaskServices).mockRejectedValue(
        new DetError(undefined, { publicMessage: `task ${TASK_ID} not found` }),
      );
      fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.queryByText('master unreachable')).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
      expect(tabLabels()).toEqual(['Logs', 'Resources']);
      expect(getTaskServices).toHaveBeenCalledTimes(6);
      await act(() => vi.advanceTimersByTimeAsync(4000));
      expect(getTaskServices).toHaveBeenCalledTimes(6);
    } finally {
      vi.useRealTimers();
    }
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
    // Nothing to kill any more.
    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await screen.findByText('Kill');
    expect(isDisabledMenuItem('Kill')).toBe(true);
  });

  it('shows why the task cannot be shown', async () => {
    vi.mocked(getTask).mockResolvedValue(undefined);
    setup();
    expect(await screen.findByText(`Task ${TASK_ID} not found.`)).toBeInTheDocument();
    expect(screen.queryByRole('tab')).not.toBeInTheDocument();
  });

  it('shows at once that the master does not know the task', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTask).mockRejectedValue(
        new DetError(undefined, { publicMessage: `task ${TASK_ID} not found` }),
      );
      setup();
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByText(`task ${TASK_ID} not found`)).toBeInTheDocument();
      expect(screen.queryByRole('tab')).not.toBeInTheDocument();
      await act(() => vi.advanceTimersByTimeAsync(4000));
      expect(getTask).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('gives up on a task that has not loaded after repeated failures, with the error', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(getTask).mockImplementation(() =>
        Promise.reject(new DetError(undefined, { publicMessage: 'master unreachable' })),
      );
      setup();
      // Four failures: at the start and after 2, 4 and 6 seconds.
      await act(() => vi.advanceTimersByTimeAsync(7000));
      expect(getTask).toHaveBeenCalledTimes(4);
      expect(screen.queryByText('master unreachable')).not.toBeInTheDocument();
      await act(() => vi.advanceTimersByTimeAsync(1000));
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByText('master unreachable')).toBeInTheDocument();
      expect(getTask).toHaveBeenCalledTimes(5);
      await act(() => vi.advanceTimersByTimeAsync(4000));
      expect(getTask).toHaveBeenCalledTimes(5);
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps polling a loaded task through failures, so that the panes follow it', async () => {
    vi.useFakeTimers();
    try {
      setup();
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-phase', 'waiting');

      vi.mocked(getTask).mockImplementation(() =>
        Promise.reject(new DetError(undefined, { publicMessage: 'master unreachable' })),
      );
      await act(() => vi.advanceTimersByTimeAsync(12000));
      expect(getTask).toHaveBeenCalledTimes(7);
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-phase', 'waiting');
      expect(screen.queryByText('master unreachable')).not.toBeInTheDocument();

      vi.mocked(getTask).mockResolvedValue(taskWith(true));
      await act(() => vi.advanceTimersByTimeAsync(2000));
      expect(getTask).toHaveBeenCalledTimes(8);
      expect(screen.getByTestId('terminal-pane')).toHaveAttribute('data-phase', 'ready');
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
