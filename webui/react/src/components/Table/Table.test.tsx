import { render, screen } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { MemoryRouter } from 'react-router-dom';

import userStore from 'stores/users';
import { BulkExperimentItem, CommandState, CommandTask, CommandType, RunState } from 'types';

import { experimentNameRenderer, taskNameRenderer } from './Table';

const shell = {
  id: 'shell-1',
  name: 'Shell (brave-otter)',
  resourcePool: 'default',
  startTime: '2026-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.Shell,
  userId: 5,
  workspaceId: 1,
} satisfies CommandTask;

const renderName = (record: CommandTask) =>
  render(<MemoryRouter>{taskNameRenderer(record.id, record, 0)}</MemoryRouter>);

describe('taskNameRenderer', () => {
  it('links a running task to its task page, in a tab named after it', () => {
    userStore.updateCurrentUser({ id: 6, isActive: true, isAdmin: false, username: 'other' });
    renderName(shell);
    const link = screen.getByText(shell.name).closest('a');
    expect(link).toHaveAttribute('href', '/tasks/shell-1/interactive');
    expect(link).toHaveAttribute('target', 'shell-1');
    // The whole name, which a narrow column cuts short.
    expect(link).toHaveAttribute('title', shell.name);
  });

  it('does not link stopped tasks', () => {
    renderName({ ...shell, id: 'shell-2', state: CommandState.Terminated });
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
  });
});

describe('job privacy', () => {
  const hidden = { manageSharing: false, viewConfig: false, viewLogs: false, viewTitle: false };

  it('names a task whose title is not shared by its kind and short ID', () => {
    renderName({
      ...shell,
      capabilities: hidden,
      id: '1a2b3c4d-0000-4000-8000-000000000000',
      name: undefined,
    });
    const link = screen.getByText('Shell 1a2b3c4d').closest('a');
    expect(link).toHaveAttribute('title', 'Shell 1a2b3c4d');
  });

  it('names an experiment whose title is not shared by its kind and ID', () => {
    const experiment: BulkExperimentItem = {
      archived: false,
      capabilities: hidden,
      hyperparameters: {},
      id: 12,
      jobId: 'job-12',
      labels: [],
      numTrials: 1,
      projectId: 1,
      resourcePool: 'default',
      searcherType: 'single',
      startTime: '2026-01-01T00:00:00Z',
      state: RunState.Active,
      userId: 5,
    };
    render(
      <UIProvider theme={DefaultTheme.Light}>
        <MemoryRouter>{experimentNameRenderer(experiment.name, experiment)}</MemoryRouter>
      </UIProvider>,
    );
    expect(screen.getByRole('link')).toHaveTextContent('Experiment 12');
  });
});
