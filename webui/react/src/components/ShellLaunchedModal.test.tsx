import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import Button from 'hew/Button';
import { useModal } from 'hew/Modal';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { BrowserRouter } from 'react-router-dom';

import { V1LaunchWarning } from 'services/api-ts-sdk';
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

const Opener: React.FC<{ response: CommandResponse }> = ({ response }) => {
  const ShellLaunchedModal = useModal(ShellLaunchedModalComponent);
  return (
    <>
      <Button onClick={ShellLaunchedModal.open}>Open</Button>
      <ShellLaunchedModal.Component response={response} />
    </>
  );
};

const setup = async (response: CommandResponse = RESPONSE) => {
  render(
    <BrowserRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <Opener response={response} />
      </UIProvider>
    </BrowserRouter>,
  );
  await userEvent.click(screen.getByRole('button', { name: 'Open' }));
  await screen.findByText('Shell Launched');
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
    expect(screen.getByText(/waits until the shell is running/)).toBeInTheDocument();

    await userEvent.click(button);
    expect(open).toHaveBeenCalledWith(
      expect.stringContaining('/shells/shell-123/terminal'),
      'shell-terminal-shell-123',
    );
    open.mockRestore();
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
