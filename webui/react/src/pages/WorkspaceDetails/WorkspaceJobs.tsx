import { PivotProps } from 'hew/Pivot';

import TaskDashboard from 'components/TaskDashboard/TaskDashboard';
import { Workspace } from 'types';

type PivotItem = NonNullable<PivotProps['items']>[number];

/** The tab keys, which are also the last part of the URL. */
export const WorkspaceJobsTab = {
  Jobs: 'jobs',
  Tasks: 'tasks',
} as const;

/**
 * The Jobs tab of a workspace (/workspaces/:id/jobs): runs of every kind in the workspace. The
 * workspace's old Tasks URL (/workspaces/:id/tasks) opens the same tab as the tasks-only view,
 * without experiments; it is a URL only, not a tab of its own.
 */
export const workspaceJobsTab = (workspace: Workspace, tab?: string): PivotItem => {
  const tasksOnly = tab === WorkspaceJobsTab.Tasks;
  return {
    children: (
      <TaskDashboard
        key={tasksOnly ? WorkspaceJobsTab.Tasks : WorkspaceJobsTab.Jobs}
        tasksOnly={tasksOnly}
        workspace={workspace}
      />
    ),
    key: tasksOnly ? WorkspaceJobsTab.Tasks : WorkspaceJobsTab.Jobs,
    label: tasksOnly ? 'Tasks' : 'Jobs',
  };
};
