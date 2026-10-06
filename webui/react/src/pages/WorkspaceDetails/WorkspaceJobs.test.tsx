import { render, screen } from '@testing-library/react';

import { Workspace, WorkspaceState } from 'types';

import { workspaceJobsTab } from './WorkspaceJobs';

vi.mock('components/TaskDashboard/TaskDashboard', () => ({
  default: ({ tasksOnly, workspace }: { tasksOnly?: boolean; workspace?: Workspace }) => (
    <div data-testid="dashboard">{`${workspace?.id} ${tasksOnly ? 'tasks only' : 'every kind'}`}</div>
  ),
}));

const WORKSPACE: Workspace = {
  archived: false,
  id: 7,
  immutable: false,
  name: 'vision',
  numExperiments: 0,
  numProjects: 0,
  pinned: false,
  state: WorkspaceState.Unspecified,
  userId: 1,
};

describe('workspaceJobsTab', () => {
  it("is the Jobs tab at /workspaces/:id/jobs, with the workspace's runs of every kind", () => {
    const tab = workspaceJobsTab(WORKSPACE, 'jobs');
    expect(tab).toMatchObject({ key: 'jobs', label: 'Jobs' });
    render(<>{tab.children}</>);
    expect(screen.getByTestId('dashboard')).toHaveTextContent('7 every kind');
  });

  it('is the Jobs tab while another tab is open, so that a click on it opens /jobs', () => {
    expect(workspaceJobsTab(WORKSPACE, 'projects')).toMatchObject({ key: 'jobs', label: 'Jobs' });
    expect(workspaceJobsTab(WORKSPACE)).toMatchObject({ key: 'jobs', label: 'Jobs' });
  });

  it('shows /workspaces/:id/tasks as the tasks-only view in the same tab', () => {
    const tab = workspaceJobsTab(WORKSPACE, 'tasks');
    expect(tab).toMatchObject({ key: 'tasks', label: 'Tasks' });
    render(<>{tab.children}</>);
    expect(screen.getByTestId('dashboard')).toHaveTextContent('7 tasks only');
  });
});
