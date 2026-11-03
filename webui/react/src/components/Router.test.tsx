import { render, screen } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import React from 'react';
import { MemoryRouter, useLocation } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import routes from 'routes/routes';
import authStore from 'stores/auth';
import { RouteConfig } from 'types';

import Router from './Router';

const OPEN_PATH = '/tasks/task-1/services/jupyter/open';

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

const setup = () =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <MemoryRouter initialEntries={[OPEN_PATH]}>
          <Router routes={testRoutes} />
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
