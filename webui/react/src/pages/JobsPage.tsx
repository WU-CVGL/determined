import React from 'react';
import { Navigate, useLocation, useParams } from 'react-router-dom';

import Page from 'components/Page';
import { RunKind } from 'components/TaskDashboard/runRows';
import TaskDashboard from 'components/TaskDashboard/TaskDashboard';
import { paths } from 'routes/utils';

interface Props {
  /** The tasks-only view at /tasks: everything but experiments. */
  tasksOnly?: boolean;
}

type Params = {
  tab?: string;
};

/** The old tab of the task list that listed generic tasks (/tasks/generic). */
const GENERIC_TAB = 'generic';

/**
 * The Jobs page (/jobs) lists runs of every kind in all workspaces the user can view. /tasks is the
 * same page without experiments, kept for bookmarks and links; its old tabs (/tasks/:tab) redirect
 * to it, /tasks/generic with the generic task filter.
 */
const JobsPage: React.FC<Props> = ({ tasksOnly = false }: Props) => {
  const { tab } = useParams<Params>();
  const location = useLocation();

  if (tasksOnly && tab) {
    const search = new URLSearchParams(location.search);
    if (tab === GENERIC_TAB) search.set('type', RunKind.GenericTask);
    const query = search.toString();
    return <Navigate replace to={`${paths.taskList()}${query ? `?${query}` : ''}`} />;
  }

  const title = tasksOnly ? 'Tasks' : 'Jobs';
  return (
    <Page
      breadcrumb={[{ breadcrumbName: title, path: tasksOnly ? paths.taskList() : paths.jobs() }]}
      id={tasksOnly ? 'tasks' : 'jobs'}
      title={title}>
      <TaskDashboard tasksOnly={tasksOnly} />
    </Page>
  );
};

export default JobsPage;
