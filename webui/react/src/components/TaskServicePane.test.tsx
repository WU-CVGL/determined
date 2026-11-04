import { render, screen } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { taskServiceUrl } from 'services/taskService';
import { TaskPhase } from 'utils/task';

import TaskServicePane from './TaskServicePane';

vi.mock('services/taskService', async (importOriginal) => ({
  ...(await importOriginal<typeof import('services/taskService')>()),
  taskServiceUrl: vi.fn(),
}));

const Pane = ({ phase }: { phase: TaskPhase }) => (
  <UIProvider theme={DefaultTheme.Light}>
    <TaskServicePane label="JupyterLab" phase={phase} service="jupyter" taskId="task-1" />
  </UIProvider>
);

describe('TaskServicePane', () => {
  beforeEach(() => {
    vi.mocked(taskServiceUrl).mockReset();
  });

  it('loads the service once the task is ready, in a frame', async () => {
    vi.mocked(taskServiceUrl).mockResolvedValue(
      'https://det.example.org/proxy/task-1:2900/?token=t',
    );
    const { rerender } = render(<Pane phase="waiting" />);
    expect(taskServiceUrl).not.toHaveBeenCalled();
    rerender(<Pane phase="ready" />);
    const frame = await screen.findByTitle('JupyterLab');
    expect(frame).toHaveAttribute('src', 'https://det.example.org/proxy/task-1:2900/?token=t');
    expect(taskServiceUrl).toHaveBeenCalledWith('task-1', 'jupyter');
    // Once: later phases do not load it again.
    rerender(<Pane phase="ended" />);
    expect(taskServiceUrl).toHaveBeenCalledTimes(1);
  });

  it('shows why the service cannot be opened, without trying again', async () => {
    vi.mocked(taskServiceUrl).mockRejectedValue(new Error('jupyter: STARTING'));
    const { rerender } = render(<Pane phase="ready" />);
    expect(await screen.findByText('jupyter: STARTING')).toBeInTheDocument();
    rerender(<Pane phase="ended" />);
    expect(taskServiceUrl).toHaveBeenCalledTimes(1);
    expect(screen.queryByTitle('JupyterLab')).not.toBeInTheDocument();
  });

  it('reads the state of a service of an ended task', async () => {
    vi.mocked(taskServiceUrl).mockRejectedValue(new Error('jupyter: EXITED'));
    render(<Pane phase="ended" />);
    expect(await screen.findByText('jupyter: EXITED')).toBeInTheDocument();
  });
});
