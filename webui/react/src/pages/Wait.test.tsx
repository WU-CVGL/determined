import { render, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { CommandState } from 'types';

import Wait from './Wait';

const mocks = vi.hoisted(() => ({
  getTask: vi.fn(),
  handleError: vi.fn(),
}));

vi.mock('services/api', () => ({ getTask: mocks.getTask }));
vi.mock('components/ThemeProvider', () => ({
  default: () => ({ actions: { hideChrome: vi.fn(), showChrome: vi.fn() } }),
}));
vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: mocks.handleError,
}));
vi.mock('routes/utils', () => ({ serverAddress: (path = '') => `http://master${path}` }));

const TASK_ID = '0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d';
const ADDRESS = `/proxy/${TASK_ID}/`;

const renderWait = (taskType: string, serviceAddr: string) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <HelmetProvider>
        <MemoryRouter initialEntries={[`/wait/${taskType}/${TASK_ID}?serviceAddr=${serviceAddr}`]}>
          <Routes>
            <Route element={<Wait />} path="/wait/:taskType/:taskId" />
          </Routes>
        </MemoryRouter>
      </HelmetProvider>
    </UIProvider>,
  );

describe('Wait', () => {
  const assign = vi.fn();

  beforeEach(() => {
    assign.mockReset();
    mocks.getTask.mockReset();
    mocks.handleError.mockReset();
    mocks.getTask.mockResolvedValue({
      allocations: [{ isReady: true, state: CommandState.Running }],
    });
    Object.defineProperty(window, 'location', { value: { assign }, writable: true });
  });

  it('opens a TensorBoard once it is ready', async () => {
    renderWait('tensor-board', ADDRESS);
    await waitFor(() => expect(assign).toHaveBeenCalled(), { timeout: 3000 });
    expect(assign).toHaveBeenCalledWith(`http://master${ADDRESS}`);
  });
});
