import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';

import userStore from 'stores/users';
import { CommandState, CommandTask, CommandType } from 'types';

import { taskNameRenderer } from './Table';

const shell: CommandTask = {
  id: 'shell-1',
  name: 'Shell (brave-otter)',
  resourcePool: 'default',
  startTime: '2026-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.Shell,
  userId: 5,
  workspaceId: 1,
};

const renderName = (record: CommandTask) =>
  render(<MemoryRouter>{taskNameRenderer(record.id, record, 0)}</MemoryRouter>);

describe('taskNameRenderer', () => {
  it('links a running shell of the user to its terminal', () => {
    userStore.updateCurrentUser({ id: 5, isActive: true, isAdmin: false, username: 'owner' });
    renderName(shell);
    const link = screen.getByText(shell.name).closest('a');
    expect(link).toHaveAttribute('href', '/shells/shell-1/terminal');
    expect(link).toHaveAttribute('target', 'shell-terminal-shell-1');
    // The whole name, which a narrow column cuts short.
    expect(link).toHaveAttribute('title', shell.name);
  });

  it('does not link other users’ or stopped shells', () => {
    userStore.updateCurrentUser({ id: 6, isActive: true, isAdmin: false, username: 'other' });
    renderName(shell);
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    userStore.updateCurrentUser({ id: 5, isActive: true, isAdmin: false, username: 'owner' });
    renderName({ ...shell, id: 'shell-2', state: CommandState.Terminated });
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
  });
});
