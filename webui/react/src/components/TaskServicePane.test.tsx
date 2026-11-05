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

const Pane = ({ ended = false, state }: { ended?: boolean; state: V1TaskServiceState }) => (
  <UIProvider theme={DefaultTheme.Light}>
    <TaskServicePane
      ended={ended}
      label="JupyterLab"
      service="jupyter"
      state={state}
      taskId="task-1"
    />
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
    rerender(<Pane state={V1TaskServiceState.READY} />);
    expect(taskServiceUrl).toHaveBeenCalledTimes(1);
  });

  it.each([V1TaskServiceState.EXITED, V1TaskServiceState.UNAVAILABLE])(
    'shows the state of a service that goes away after it loaded (%s)',
    async (gone) => {
      vi.mocked(taskServiceUrl).mockResolvedValueOnce('https://det.example.org/proxy/task-1:2900/');
      vi.mocked(taskServiceUrl).mockRejectedValueOnce(
        new Error(`jupyter: ${gone.replace('STATE_', '')}`),
      );
      const { rerender } = render(<Pane state={V1TaskServiceState.READY} />);
      expect(await screen.findByTitle('JupyterLab')).toBeInTheDocument();
      rerender(<Pane state={gone} />);
      const text = `jupyter: ${gone.replace('STATE_', '')}`;
      expect(screen.getByText(text)).toBeInTheDocument();
      expect(screen.queryByTitle('JupyterLab')).not.toBeInTheDocument();
      expect(taskServiceUrl).toHaveBeenCalledTimes(1);

      await userEvent.click(screen.getByRole('button', { name: 'Reconnect' }));
      expect(await screen.findByText(text)).toBeInTheDocument();
      expect(taskServiceUrl).toHaveBeenCalledTimes(2);
    },
  );

  it('opens a service without a credential, such as TensorBoard', async () => {
    vi.mocked(taskServiceUrl).mockResolvedValue('https://det.example.org/proxy/task-1:2902/');
    render(
      <UIProvider theme={DefaultTheme.Light}>
        <TaskServicePane
          ended={false}
          label="TensorBoard"
          service="tensorboard"
          state={V1TaskServiceState.READY}
          taskId="task-1"
        />
      </UIProvider>,
    );
    const frame = await screen.findByTitle('TensorBoard');
    expect(frame).toHaveAttribute('src', 'https://det.example.org/proxy/task-1:2902/');
    expect(taskServiceUrl).toHaveBeenCalledWith('task-1', 'tensorboard');
  });

  it('shows why the service cannot be opened, without trying again on its own', async () => {
    vi.mocked(taskServiceUrl).mockRejectedValue(new Error('jupyter: login failed'));
    const { rerender } = render(<Pane state={V1TaskServiceState.READY} />);
    expect(await screen.findByText('jupyter: login failed')).toBeInTheDocument();
    rerender(<Pane state={V1TaskServiceState.STARTING} />);
    rerender(<Pane state={V1TaskServiceState.READY} />);
    expect(taskServiceUrl).toHaveBeenCalledTimes(1);
    expect(screen.queryByTitle('JupyterLab')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reconnect' })).toBeInTheDocument();
  });

  it('tries once more on Reconnect, and shows a second failure with Reconnect again', async () => {
    vi.mocked(taskServiceUrl).mockRejectedValueOnce(new Error('jupyter: login failed'));
    vi.mocked(taskServiceUrl).mockRejectedValueOnce(new Error('jupyter: EXITED'));
    vi.mocked(taskServiceUrl).mockResolvedValueOnce('https://det.example.org/proxy/task-1:2900/');
    render(<Pane state={V1TaskServiceState.READY} />);
    expect(await screen.findByText('jupyter: login failed')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Reconnect' }));
    expect(await screen.findByText('jupyter: EXITED')).toBeInTheDocument();
    expect(taskServiceUrl).toHaveBeenCalledTimes(2);
    expect(screen.queryByText('jupyter: login failed')).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Reconnect' }));
    const frame = await screen.findByTitle('JupyterLab');
    expect(frame).toHaveAttribute('src', 'https://det.example.org/proxy/task-1:2900/');
    expect(taskServiceUrl).toHaveBeenCalledTimes(3);
    expect(screen.queryByRole('button', { name: 'Reconnect' })).not.toBeInTheDocument();
  });

  it('shows the end of a task over the last state read, without Reconnect', async () => {
    vi.mocked(taskServiceUrl).mockResolvedValue('https://det.example.org/proxy/task-1:2900/');
    const { rerender } = render(<Pane state={V1TaskServiceState.READY} />);
    expect(await screen.findByTitle('JupyterLab')).toBeInTheDocument();
    rerender(<Pane ended state={V1TaskServiceState.READY} />);
    expect(screen.getByText('jupyter: EXITED')).toBeInTheDocument();
    expect(screen.queryByTitle('JupyterLab')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Reconnect' })).not.toBeInTheDocument();
    expect(taskServiceUrl).toHaveBeenCalledTimes(1);
  });

  it.each([
    [V1TaskServiceState.READY, 'jupyter: EXITED'],
    [V1TaskServiceState.UNAVAILABLE, 'jupyter: UNAVAILABLE'],
  ])('reads nothing for an ended task (%s)', (state, text) => {
    render(<Pane ended state={state} />);
    expect(screen.getByText(text)).toBeInTheDocument();
    expect(screen.queryByTitle('JupyterLab')).not.toBeInTheDocument();
    expect(taskServiceUrl).not.toHaveBeenCalled();
  });

  it('reads the state of a service that has exited or cannot run', async () => {
    vi.mocked(taskServiceUrl).mockRejectedValue(new Error('jupyter: EXITED'));
    render(<Pane state={V1TaskServiceState.EXITED} />);
    expect(await screen.findByText('jupyter: EXITED')).toBeInTheDocument();
    expect(taskServiceUrl).toHaveBeenCalledTimes(1);
  });
});
