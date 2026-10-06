import { render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import routes from 'routes/routes';
import { paths } from 'routes/utils';

import JobsPage from './JobsPage';

vi.mock('components/TaskDashboard/TaskDashboard', () => ({
  default: ({ tasksOnly }: { tasksOnly?: boolean }) => (
    <div data-testid="dashboard">{tasksOnly ? 'tasks only' : 'every kind'}</div>
  ),
}));

const Location = () => {
  const location = useLocation();
  return <div data-testid="location">{`${location.pathname}${location.search}`}</div>;
};

const setup = (url: string) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <HelmetProvider>
          <MemoryRouter initialEntries={[url]}>
            <Routes>
              <Route element={<JobsPage />} path="/jobs" />
              <Route element={<JobsPage tasksOnly />} path="/tasks/:tab" />
              <Route element={<JobsPage tasksOnly />} path="/tasks" />
            </Routes>
            <Location />
          </MemoryRouter>
        </HelmetProvider>
      </ThemeProvider>
    </UIProvider>,
  );

describe('JobsPage', () => {
  it('lists runs of every kind at /jobs', async () => {
    setup('/jobs');
    expect(await screen.findByTestId('dashboard')).toHaveTextContent('every kind');
    expect(screen.getByRole('link', { name: 'Jobs' })).toHaveAttribute('href', '/jobs');
    expect(screen.queryByRole('tab')).not.toBeInTheDocument();
  });

  it('is the tasks-only view at /tasks', async () => {
    setup('/tasks');
    expect(await screen.findByTestId('dashboard')).toHaveTextContent('tasks only');
    expect(screen.getByRole('link', { name: 'Tasks' })).toHaveAttribute('href', '/tasks');
  });

  it('redirects /tasks/generic to the tasks-only view with the generic task filter', async () => {
    setup('/tasks/generic');
    await waitFor(() =>
      expect(screen.getByTestId('location')).toHaveTextContent('/tasks?type=generic-task'),
    );
    expect(await screen.findByTestId('dashboard')).toHaveTextContent('tasks only');
  });

  it('redirects the other old tabs to /tasks and keeps their kind filter', async () => {
    setup('/tasks/interactive?type=jupyter-lab');
    await waitFor(() =>
      expect(screen.getByTestId('location')).toHaveTextContent('/tasks?type=jupyter-lab'),
    );
    expect(await screen.findByTestId('dashboard')).toHaveTextContent('tasks only');
  });

  it('redirects an old tab without a filter to /tasks', async () => {
    setup('/tasks/interactive');
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent(/^\/tasks$/));
  });
});

describe('Jobs routes', () => {
  it('serve the Jobs page at /jobs, no longer a redirect to the cluster page', () => {
    const jobs = routes.find((route) => route.id === 'jobs');
    expect(jobs).toMatchObject({ path: '/jobs', title: 'Jobs' });
    expect(jobs?.redirect).toBeUndefined();
    expect(paths.jobs()).toBe('/jobs');
  });

  it('keep /tasks and its old tabs, and the task pages under /tasks', () => {
    const taskRoutes = routes.filter((route) => route.id === 'taskList').map((r) => r.path);
    expect(taskRoutes).toEqual(['/tasks/:tab', '/tasks']);
    expect(routes.find((route) => route.id === 'taskResources')?.path).toBe(
      '/tasks/:taskId/resources',
    );
    expect(paths.taskList()).toBe('/tasks');
  });
});
