import { render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';

import { TaskLogsWrapper } from './TaskLogs';

const TASK_ID = '0123abcd-4567-89ef';

vi.mock('services/api', () => ({
  getTask: vi.fn(() => Promise.reject(new Error('no task record'))),
}));
vi.mock('services/apiConfig', () => ({
  detApi: { StreamingJobs: { taskLogs: vi.fn(), taskLogsFields: vi.fn() } },
}));
vi.mock('services/utils', () => ({ readStream: vi.fn() }));
vi.mock('hew/LogViewer/LogViewer', async (importOriginal) => ({
  ...(await importOriginal<typeof import('hew/LogViewer/LogViewer')>()),
  default: () => <div data-testid="log-viewer" />,
}));
vi.mock('hooks/useSettings', () => ({
  useSettings: () => ({ resetSettings: vi.fn(), settings: {}, updateSettings: vi.fn() }),
}));
vi.mock('hooks/usePermissions', () => ({ default: () => ({ loading: false }) }));

const setup = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <HelmetProvider>
            <Routes>
              <Route element={<TaskLogsWrapper />} path="/:taskType/:taskId/logs" />
            </Routes>
          </HelmetProvider>
        </ThemeProvider>
      </UIProvider>
    </MemoryRouter>,
  );

describe('TaskLogs', () => {
  it('titles the page from the task type and ID, not from the address', async () => {
    setup(`/shell/${TASK_ID}/logs?id=<script>secret`);
    await screen.findByTestId('log-viewer');
    await waitFor(() => expect(document.title).toMatch(/^Shell 0123abcd Logs - /));
    expect(document.title).not.toContain('secret');
    expect(document.title).not.toContain('<script>');
    expect(document.body.innerHTML).not.toContain('secret');
    expect(screen.getByRole('link', { name: 'Shell 0123abcd' })).toHaveAttribute(
      'href',
      `/shell/${TASK_ID}/logs`,
    );
  });
});
