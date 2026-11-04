import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { V1TaskServiceState } from 'services/api-ts-sdk';
import { taskServiceUrl } from 'services/taskService';

import TaskServicePane from './TaskServicePane';

vi.mock('services/taskService', async (importOriginal) => ({
  ...(await importOriginal<typeof import('services/taskService')>()),
  taskServiceUrl: vi.fn(),
}));

const Pane = ({ state }: { state: V1TaskServiceState }) => (
  <UIProvider theme={DefaultTheme.Light}>
    <TaskServicePane label="JupyterLab" service="jupyter" state={state} taskId="task-1" />
  </UIProvider>
);

describe('TaskServicePane', () => {
  beforeEach(() => {
    vi.mocked(taskServiceUrl).mockReset();
  });

  it('loads the service once it is ready, in a frame', async () => {
    vi.mocked(taskServiceUrl).mockResolvedValue(
      'https://det.example.org/proxy/task-1:2900/?token=t',
    );
    const { rerender } = render(<Pane state={V1TaskServiceState.UNSPECIFIED} />);
    rerender(<Pane state={V1TaskServiceState.STARTING} />);
    expect(taskServiceUrl).not.toHaveBeenCalled();
    rerender(<Pane state={V1TaskServiceState.READY} />);
    const frame = await screen.findByTitle('JupyterLab');
    expect(frame).toHaveAttribute('src', 'https://det.example.org/proxy/task-1:2900/?token=t');
    expect(taskServiceUrl).toHaveBeenCalledWith('task-1', 'jupyter');
    // Once: later states do not load it again.
    rerender(<Pane state={V1TaskServiceState.EXITED} />);
    expect(taskServiceUrl).toHaveBeenCalledTimes(1);
  });

  it('shows why the service cannot be opened, without trying again on its own', async () => {
    vi.mocked(taskServiceUrl).mockRejectedValue(new Error('jupyter: login failed'));
    const { rerender } = render(<Pane state={V1TaskServiceState.READY} />);
    expect(await screen.findByText('jupyter: login failed')).toBeInTheDocument();
    rerender(<Pane state={V1TaskServiceState.STARTING} />);
    rerender(<Pane state={V1TaskServiceState.READY} />);
    expect(taskServiceUrl).toHaveBeenCalledTimes(1);
    expect(screen.queryByTitle('JupyterLab')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('tries once more on Retry, and shows a second failure with Retry again', async () => {
    vi.mocked(taskServiceUrl).mockRejectedValueOnce(new Error('jupyter: login failed'));
    vi.mocked(taskServiceUrl).mockRejectedValueOnce(new Error('jupyter: EXITED'));
    vi.mocked(taskServiceUrl).mockResolvedValueOnce('https://det.example.org/proxy/task-1:2900/');
    render(<Pane state={V1TaskServiceState.READY} />);
    expect(await screen.findByText('jupyter: login failed')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('jupyter: EXITED')).toBeInTheDocument();
    expect(taskServiceUrl).toHaveBeenCalledTimes(2);
    expect(screen.queryByText('jupyter: login failed')).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
    const frame = await screen.findByTitle('JupyterLab');
    expect(frame).toHaveAttribute('src', 'https://det.example.org/proxy/task-1:2900/');
    expect(taskServiceUrl).toHaveBeenCalledTimes(3);
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
  });

  it('reads the state of a service that has exited or cannot run', async () => {
    vi.mocked(taskServiceUrl).mockRejectedValue(new Error('jupyter: EXITED'));
    render(<Pane state={V1TaskServiceState.EXITED} />);
    expect(await screen.findByText('jupyter: EXITED')).toBeInTheDocument();
    expect(taskServiceUrl).toHaveBeenCalledTimes(1);
  });
});
