import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import Button from 'hew/Button';
import { useModal } from 'hew/Modal';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { BrowserRouter } from 'react-router-dom';

import {
  TaskServiceProtocol,
  V1LaunchWarning,
  V1TaskService,
  V1TaskServiceState,
} from 'services/api-ts-sdk';
import { CommandResponse, CommandState, CommandType } from 'types';

import ShellLaunchedModalComponent from './ShellLaunchedModal';

const flags = vi.hoisted(() => ({ resources: true as boolean | undefined, terminal: true }));

vi.mock('hooks/useFeature', () => ({
  default: () => ({ isOn: (feature: string) => feature === 'shell_terminal' && flags.terminal }),
}));
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => flags.resources }));

const RESPONSE: CommandResponse = {
  command: {
    id: 'shell-123',
    name: 'Shell (lively-calm-fox)',
    resourcePool: 'default',
    startTime: '2026-01-01T00:00:00Z',
    state: CommandState.Queued,
    type: CommandType.Shell,
    userId: 1,
    workspaceId: 3,
  },
  warnings: [],
};

/** A service of the launched task as the launching hook passes it, just started. */
const service = (name: string, mayConnect = true): V1TaskService => ({
  mayConnect,
  name,
  protocol: name === 'ssh' ? TaskServiceProtocol.TCP : TaskServiceProtocol.HTTP,
  serviceId: name === 'ssh' ? 'shell-123' : 'shell-123:2900',
  state: V1TaskServiceState.STARTING,
  url: name === 'ssh' ? '' : '/proxy/shell-123:2900/',
});
/** A shell's SSH alone, the default. */
const SSH_ONLY = [service('ssh')];
const ALL_SERVICES = [service('ssh'), service('jupyter'), service('vscode')];

interface OpenerProps {
  response: CommandResponse;
  services: V1TaskService[];
}

const Opener: React.FC<OpenerProps> = ({ response, services }) => {
  const ShellLaunchedModal = useModal(ShellLaunchedModalComponent);
  return (
    <>
      <Button onClick={ShellLaunchedModal.open}>Open</Button>
      <ShellLaunchedModal.Component response={response} services={services} />
    </>
  );
};

const setup = async (response: CommandResponse = RESPONSE, services = SSH_ONLY) => {
  render(
    <BrowserRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <Opener response={response} services={services} />
      </UIProvider>
    </BrowserRouter>,
  );
  await userEvent.click(screen.getByRole('button', { name: 'Open' }));
  await screen.findByText('Interactive Task Launched');
};

describe('ShellLaunchedModal', () => {
  beforeEach(() => {
    flags.resources = true;
    flags.terminal = true;
  });

  it('offers Open Terminal before the CLI command and opens the shell’s terminal tab', async () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    await setup();
    const button = screen.getByRole('button', { name: 'Open Terminal' });
    const cliLabel = screen.getByText('Start an interactive SSH session in the terminal:');
    expect(
      button.compareDocumentPosition(cliLabel) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(button.closest('button')).toHaveClass('ant-btn-primary');
    expect(screen.queryByText(/waits until the shell is running/)).not.toBeInTheDocument();

    await userEvent.click(button);
    expect(open).toHaveBeenCalledWith(
      expect.stringContaining('/tasks/shell-123/interactive?tab=terminal'),
      'shell-123',
      '',
    );
    open.mockRestore();
  });

  it('offers Open JupyterLab and Open Visual Studio Code for the services of the task', async () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    await setup(RESPONSE, ALL_SERVICES);
    const jupyter = screen.getByRole('button', { name: 'Open JupyterLab' });
    const vscode = screen.getByRole('button', { name: 'Open Visual Studio Code' });
    const terminal = screen.getByRole('button', { name: 'Open Terminal' });
    expect(
      terminal.compareDocumentPosition(jupyter) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(jupyter.compareDocumentPosition(vscode) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(terminal).toHaveClass('ant-btn-primary');
    expect(jupyter).not.toHaveClass('ant-btn-primary');

    await userEvent.click(jupyter);
    expect(open).toHaveBeenLastCalledWith(
      expect.stringContaining('/tasks/shell-123/interactive?tab=jupyter'),
      'shell-123',
      '',
    );
    await userEvent.click(vscode);
    expect(open).toHaveBeenLastCalledWith(
      expect.stringContaining('/tasks/shell-123/interactive?tab=vscode'),
      'shell-123',
      '',
    );
    open.mockRestore();
  });

  it('offers the service buttons without the terminal when the master turns it off', async () => {
    flags.terminal = false;
    await setup(RESPONSE, [service('ssh'), service('vscode')]);
    expect(screen.getByRole('button', { name: 'Open Visual Studio Code' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: 'Open Terminal' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Open JupyterLab' })).not.toBeInTheDocument();
  });

  it('shows no service buttons for a shell with SSH alone', async () => {
    await setup();
    expect(screen.getByRole('button', { name: 'Open Terminal' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Open JupyterLab' })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: 'Open Visual Studio Code' }),
    ).not.toBeInTheDocument();
  });

  it('shows no service buttons without services, as after a failed read', async () => {
    await setup(RESPONSE, []);
    expect(screen.getByRole('button', { name: 'Open Terminal' })).toBeInTheDocument();
    expect(screen.getByText('det shell open shell-123')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Open JupyterLab' })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: 'Open Visual Studio Code' }),
    ).not.toBeInTheDocument();
  });

  it('shows no service buttons for a service the user may not connect to', async () => {
    await setup(RESPONSE, [
      service('ssh', false),
      service('jupyter', false),
      service('vscode', false),
    ]);
    expect(screen.queryByRole('button', { name: 'Open JupyterLab' })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: 'Open Visual Studio Code' }),
    ).not.toBeInTheDocument();
  });

  it('links View Resources next to View Logs', async () => {
    await setup();
    const logs = screen.getByText('View Logs').closest('a');
    const resources = screen.getByText('View Resources').closest('a');
    expect(logs).toHaveAttribute('href', expect.stringContaining('/shell/shell-123/logs'));
    expect(resources).toHaveAttribute(
      'href',
      expect.stringContaining('/tasks/shell-123/resources'),
    );
    expect(logs?.parentElement).toBe(resources?.parentElement);
  });

  it('leaves out Open Terminal when the master turns the terminal off', async () => {
    flags.terminal = false;
    await setup();
    expect(screen.queryByRole('button', { name: 'Open Terminal' })).not.toBeInTheDocument();
    expect(screen.getByText('det shell open shell-123')).toBeInTheDocument();
  });

  it.each([false, undefined])(
    'leaves out View Resources when the master does not offer it (%s)',
    async (enabled) => {
      flags.resources = enabled;
      await setup();
      expect(screen.getByText('View Logs')).toBeInTheDocument();
      expect(screen.queryByText('View Resources')).not.toBeInTheDocument();
    },
  );

  it('warns when the shell may wait in the queue', async () => {
    await setup({ ...RESPONSE, warnings: [V1LaunchWarning.CURRENTSLOTSEXCEEDED] });
    expect(screen.getByText(/may wait in the queue/)).toBeInTheDocument();
  });
});
