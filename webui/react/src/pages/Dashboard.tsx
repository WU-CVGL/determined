import Breadcrumb from 'hew/Breadcrumb';
import Card from 'hew/Card';
import Icon from 'hew/Icon';
import Message from 'hew/Message';
import Row from 'hew/Row';
import Spinner from 'hew/Spinner';
import { Loadable } from 'hew/utils/loadable';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import ExperimentIcons from 'components/ExperimentIcons';
import Link from 'components/Link';
import Page, { BreadCrumbRoute } from 'components/Page';
import ProjectCard from 'components/ProjectCard';
import Section from 'components/Section';
import ShellButton from 'components/ShellButton';
import ResponsiveTable from 'components/Table/ResponsiveTable';
import {
  experimentNameRenderer,
  relativeTimeRenderer,
  taskNameRenderer,
  taskTypeRenderer,
} from 'components/Table/Table';
import useFeature from 'hooks/useFeature';
import usePermissions from 'hooks/usePermissions';
import usePolling from 'hooks/usePolling';
import UserOverview from 'pages/UserProfile/UserOverview';
import { paths } from 'routes/utils';
import { getCommands, getExperiments, getProjectsByUserActivity, getShells } from 'services/api';
import userStore from 'stores/users';
import workspaceStore from 'stores/workspaces';
import { BulkExperimentItem, CommandTask, DetailedUser, Project } from 'types';
import handleError, { ErrorType } from 'utils/error';
import { useObservable } from 'utils/observable';
import { dateTimeStringSorter } from 'utils/sort';

import css from './Dashboard.module.scss';

const SUBMISSIONS_FETCH_LIMIT = 25;
const PROJECTS_FETCH_LIMIT = 5;
const DISPLAY_LIMIT = 25;

const Dashboard: React.FC = () => {
  const [experiments, setExperiments] = useState<BulkExperimentItem[]>([]);
  const [tasks, setTasks] = useState<CommandTask[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [canceler] = useState(new AbortController());
  const [submissionsLoading, setSubmissionsLoading] = useState<boolean>(true);
  const [projectsLoading, setProjectsLoading] = useState<boolean>(true);
  // A restart of the polling starts a batch while the last one is pending; only the latest batch
  // sets the state.
  const batch = useRef(0);
  const currentUser = Loadable.getOrElse(undefined, useObservable(userStore.currentUser));
  const workspaces = Loadable.getOrElse([], useObservable(workspaceStore.workspaces));
  const { canCreateNSC } = usePermissions();
  const f_flat_runs = useFeature().isOn('flat_runs');
  type Submission = BulkExperimentItem & CommandTask;

  const fetchTasks = useCallback(
    async (user: DetailedUser, isCurrent: () => boolean) => {
      const results = await Promise.allSettled([
        getCommands({
          limit: SUBMISSIONS_FETCH_LIMIT,
          orderBy: 'ORDER_BY_DESC',
          signal: canceler.signal,
          sortBy: 'SORT_BY_START_TIME',
          users: [user.id.toString()],
        }),
        getShells({
          limit: SUBMISSIONS_FETCH_LIMIT,
          orderBy: 'ORDER_BY_DESC',
          signal: canceler.signal,
          sortBy: 'SORT_BY_START_TIME',
          users: [user.id.toString()],
        }),
      ]);
      const newTasks = results.reduce((acc, current) => {
        if (current.status === 'fulfilled') return acc.concat(current.value);
        return acc;
      }, [] as CommandTask[]);
      if (isCurrent()) setTasks(newTasks);
    },
    [canceler],
  );

  const fetchExperiments = useCallback(
    async (user: DetailedUser, isCurrent: () => boolean) => {
      try {
        const response = await getExperiments(
          {
            limit: SUBMISSIONS_FETCH_LIMIT,
            orderBy: 'ORDER_BY_DESC',
            sortBy: 'SORT_BY_START_TIME',
            users: [user.id.toString()],
          },
          {
            signal: canceler.signal,
          },
        );
        if (isCurrent()) setExperiments(response.experiments);
      } catch (e) {
        handleError(e, {
          publicSubject: `Error fetching ${f_flat_runs ? 'searches' : 'experiments'} for dashboard`,
          silent: false,
          type: ErrorType.Api,
        });
      }
    },
    [f_flat_runs, canceler],
  );

  const fetchProjects = useCallback(
    async (isCurrent: () => boolean) => {
      try {
        const projects = await getProjectsByUserActivity(
          { limit: PROJECTS_FETCH_LIMIT },
          {
            signal: canceler.signal,
          },
        );
        if (!isCurrent()) return;
        setProjects(projects);
        setProjectsLoading(false);
      } catch (e) {
        handleError(e, {
          publicSubject: 'Error fetching projects for dashboard',
          silent: false,
          type: ErrorType.Api,
        });
      }
    },
    [canceler],
  );

  const fetchSubmissions = useCallback(
    async (isCurrent: () => boolean) => {
      if (!currentUser) return;
      await Promise.allSettled([
        fetchExperiments(currentUser, isCurrent),
        fetchTasks(currentUser, isCurrent),
      ]);
      if (isCurrent()) setSubmissionsLoading(false);
    },
    [currentUser, fetchExperiments, fetchTasks],
  );

  // The polling schedules the next batch once this one has settled.
  const fetchAll = useCallback(async () => {
    const current = ++batch.current;
    const isCurrent = () => batch.current === current;
    await Promise.allSettled([fetchProjects(isCurrent), fetchSubmissions(isCurrent)]);
  }, [fetchSubmissions, fetchProjects]);

  const { stopPolling } = usePolling(fetchAll, { rerunOnNewFn: true });

  const submissions = useMemo(
    () =>
      (experiments as Submission[])
        .concat(tasks as Submission[])
        .sort((a, b) => dateTimeStringSorter(b.startTime, a.startTime))
        .slice(0, DISPLAY_LIMIT),
    [experiments, tasks],
  );

  useEffect(() => {
    return () => {
      canceler.abort();
      stopPolling();
    };
  }, [canceler, stopPolling]);

  const pageBreadCrumb: BreadCrumbRoute[] = [{ breadcrumbName: 'Home', path: paths.dashboard() }];

  return (
    <Page
      breadcrumb={pageBreadCrumb}
      options={
        <Row>
          <ShellButton enabled={canCreateNSC} />
        </Row>
      }
      title="Home">
      {currentUser && <UserOverview userId={currentUser.id} />}
      {projectsLoading ? (
        <Section>
          <Spinner center spinning />
        </Section>
      ) : projects.length > 0 ? (
        // hide Projects header when empty:
        <Section title="Recently Viewed Projects">
          <Card.Group size="small" wrap={false}>
            {projects.map((project) => (
              <ProjectCard hideActionMenu key={project.id} project={project} showWorkspace />
            ))}
          </Card.Group>
        </Section>
      ) : null}
      {/* show Submissions header even when empty: */}
      <Section title="Your Recent Submissions">
        {submissionsLoading ? (
          <Spinner center spinning />
        ) : submissions.length > 0 ? (
          <ResponsiveTable<Submission>
            className={css.table}
            columns={[
              {
                dataIndex: 'state',
                render: (state) => {
                  return <ExperimentIcons state={state} />;
                },
                width: 1,
              },
              {
                dataIndex: 'projectId',
                render: (projectId, row, index) => {
                  if (projectId) {
                    return <Icon name="experiment" title={f_flat_runs ? 'Experiment' : 'Search'} />;
                  } else {
                    return taskTypeRenderer(row.type, row, index);
                  }
                },
                width: 1,
              },
              {
                dataIndex: 'name',
                render: (name, row, index) => {
                  if (row.projectId) {
                    return experimentNameRenderer(name, row);
                  } else {
                    return taskNameRenderer(row.id, row, index);
                  }
                },
              },
              {
                dataIndex: 'projectId',
                render: (projectId, row) => {
                  if (row.workspaceId && !row.projectId) {
                    // Tasks
                    return (
                      <Breadcrumb>
                        <Breadcrumb.Item>
                          <Link path={paths.workspaceDetails(row.workspaceId)}>
                            {workspaces.find((w) => w.id === row.workspaceId)?.name ||
                              String(row.workspaceId)}
                          </Link>
                        </Breadcrumb.Item>
                      </Breadcrumb>
                    );
                  } else if (row.workspaceId && row.projectId !== 1) {
                    return (
                      <Breadcrumb>
                        <Breadcrumb.Item>
                          <Link path={paths.workspaceDetails(row.workspaceId)}>
                            {row.workspaceName}
                          </Link>
                        </Breadcrumb.Item>
                        <Breadcrumb.Item>
                          <Link path={paths.projectDetails(projectId)}>{row.projectName}</Link>
                        </Breadcrumb.Item>
                      </Breadcrumb>
                    );
                  }
                  if (row.projectName) {
                    return (
                      <Breadcrumb>
                        <Breadcrumb.Item>
                          <Link path={paths.projectDetails(projectId)}>{row.projectName}</Link>
                        </Breadcrumb.Item>
                      </Breadcrumb>
                    );
                  }
                  return <></>;
                },
              },
              {
                dataIndex: 'startTime',
                render: relativeTimeRenderer,
              },
            ]}
            dataSource={submissions}
            loading={submissionsLoading}
            pagination={false}
            rowKey="id"
            showHeader={false}
            size="middle"
          />
        ) : (
          <Message
            description={
              <>
                Your recent {f_flat_runs ? 'searches' : 'experiments'} and tasks will show up here.{' '}
                <Link external path={paths.docs('/quickstart-mdldev.html')}>
                  Get started
                </Link>
              </>
            }
            icon="experiment"
            title="No submissions"
          />
        )}
      </Section>
    </Page>
  );
};

export default Dashboard;
