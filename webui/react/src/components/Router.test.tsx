import { render, screen } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import React from 'react';
import { MemoryRouter, useLocation } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import InteractiveTaskRedirect from 'pages/InteractiveTaskRedirect';
import routes from 'routes/routes';
import authStore from 'stores/auth';
import { InteractiveTaskTab, RouteConfig } from 'types';

import Router from './Router';

const OPEN_PATH = '/tasks/task-1/services/jupyter/open';
const TASK_PATH = '/tasks/task-1/interactive';

const LocationProbe: React.FC = () => {
  const location = useLocation();
  return <div data-testid="location">{`${location.pathname}${location.search}`}</div>;
};

const LoginProbe: React.FC = () => {
  const location = useLocation();
  const from = (location.state as { pathname?: string } | null)?.pathname;
  return <div>login from {from}</div>;
};

const openRoute = routes.find((r) => r.id === 'taskServiceOpen') as RouteConfig;

const testRoutes: RouteConfig[] = [
  { ...openRoute, element: <div>service page</div> },
  { element: <LoginProbe />, id: 'signIn', needAuth: false, path: '/login' },
];

const routeById = (id: string) => routes.find((r) => r.id === id) as RouteConfig;

/* The task page and the two old routes that send to it, with their real redirect components. */
const taskRoutes: RouteConfig[] = [
  { ...routeById('interactiveTask'), element: <LocationProbe /> },
  { ...routeById('interactive'), element: <InteractiveTaskRedirect /> },
  {
    ...routeById('shellTerminal'),
    element: <InteractiveTaskRedirect tab={InteractiveTaskTab.Terminal} />,
  },
];

const setup = (path = OPEN_PATH, withRoutes = testRoutes) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <MemoryRouter initialEntries={[path]}>
          <Router routes={withRoutes} />
        </MemoryRouter>
      </ThemeProvider>
    </UIProvider>,
  );

describe('Router', () => {
  afterEach(() => authStore.reset());

  it('needs a login for the page that opens a task service', () => {
    expect(openRoute.needAuth).toBe(true);
    expect(openRoute.path).toBe('/tasks/:taskId/services/:service/open');
  });

  it('sends a visit without a login to the login page with the path to return to', async () => {
    authStore.setAuth({ isAuthenticated: false });
    setup();
    expect(await screen.findByText(`login from ${OPEN_PATH}`)).toBeInTheDocument();
    expect(screen.queryByText('service page')).not.toBeInTheDocument();
  });

  it('shows the page after a login', async () => {
    authStore.setAuth({ isAuthenticated: true });
    setup();
    expect(await screen.findByText('service page')).toBeInTheDocument();
  });
});

describe('task page routes', () => {
  beforeEach(() => authStore.setAuth({ isAuthenticated: true }));
  afterEach(() => authStore.reset());

  it('serve the task page by ID, behind a login', () => {
    const route = routeById('interactiveTask');
    expect(route.needAuth).toBe(true);
    expect(route.path).toBe('/tasks/:taskId/interactive');
    expect(routeById('interactive').needAuth).toBe(true);
    expect(routeById('shellTerminal').needAuth).toBe(true);
  });

  it('send the old interactive page of the WebUI to the task page', async () => {
    const waitUrl = encodeURIComponent('/det/wait/tensor-board/task-1?serviceAddr=/proxy/task-1/');
    setup(
      `/interactive/task-1/tensor-board/TensorBoard%20(x)/default/${waitUrl}/?currentSlotsExceeded=false`,
      taskRoutes,
    );
    expect(await screen.findByTestId('location')).toHaveTextContent(TASK_PATH);
    expect(screen.getByTestId('location')).toHaveTextContent(/interactive$/);
  });

  it('send the old interactive page as the CLI builds it to the task page, with its warning', async () => {
    // quote_plus of the wait page URL, with the CLI's own type segment and no trailing slash.
    const waitUrl =
      '%2Fdet%2Fwait%2Ftensorboard%2Ftask-1%3FeventUrl%3D%2Fproxy%2Ftask-1%2Ftensorboard%2Ftask-1%2Fevents%3Ftail%3D1%26serviceAddr%3D%2Fproxy%2Ftask-1';
    setup(
      `/interactive/task-1/tensorboard/TensorBoard+%28x%29/default/${waitUrl}?currentSlotsExceeded=true`,
      taskRoutes,
    );
    expect(await screen.findByTestId('location')).toHaveTextContent(
      `${TASK_PATH}?currentSlotsExceeded=true`,
    );
  });

  it('send the old terminal page to the Terminal tab of the task page', async () => {
    setup('/shells/task-1/terminal', taskRoutes);
    expect(await screen.findByTestId('location')).toHaveTextContent(`${TASK_PATH}?tab=terminal`);
  });
});
