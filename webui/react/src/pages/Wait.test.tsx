import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { CommandState } from 'types';
import { NOTEBOOK_ACCESS_DENIED } from 'utils/wait';

import Wait from './Wait';

const mocks = vi.hoisted(() => ({
  getJupyterLab: vi.fn(),
  getTask: vi.fn(),
  handleError: vi.fn(),
}));

vi.mock('services/api', () => ({ getJupyterLab: mocks.getJupyterLab, getTask: mocks.getTask }));
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
const ADDRESS_WITH_TOKEN = `${ADDRESS}?token=jupyter-token`;

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
    mocks.getJupyterLab.mockReset();
    mocks.getTask.mockReset();
    mocks.handleError.mockReset();
    mocks.getTask.mockResolvedValue({
      allocations: [{ isReady: true, state: CommandState.Running }],
    });
    Object.defineProperty(window, 'location', { value: { assign }, writable: true });
  });

  it('opens a notebook whose address already carries its token', async () => {
    renderWait('jupyter-lab', ADDRESS_WITH_TOKEN);
    await waitFor(() => expect(assign).toHaveBeenCalled(), { timeout: 3000 });
    expect(assign).toHaveBeenCalledWith(`http://master${ADDRESS_WITH_TOKEN}`);
    expect(mocks.getJupyterLab).not.toHaveBeenCalled();
  });

  it('fetches the token of a notebook opened from a listing', async () => {
    mocks.getJupyterLab.mockResolvedValue({ serviceAddress: ADDRESS_WITH_TOKEN });
    renderWait('jupyter-lab', ADDRESS);
    await waitFor(() => expect(assign).toHaveBeenCalled(), { timeout: 3000 });
    expect(mocks.getJupyterLab).toHaveBeenCalledWith({ commandId: TASK_ID });
    expect(assign).toHaveBeenCalledWith(`http://master${ADDRESS_WITH_TOKEN}`);
  });

  it('refuses a notebook that the user may not open', async () => {
    mocks.getJupyterLab.mockResolvedValue({ serviceAddress: ADDRESS });
    renderWait('jupyter-lab', ADDRESS);
    expect(await screen.findByText(NOTEBOOK_ACCESS_DENIED, {}, { timeout: 3000 })).toBeVisible();
    expect(assign).not.toHaveBeenCalled();
  });

  it('offers a retry when the notebook address fails to load', async () => {
    mocks.getJupyterLab
      .mockRejectedValueOnce(new Error('Service Unavailable'))
      .mockResolvedValueOnce({ serviceAddress: ADDRESS_WITH_TOKEN });
    renderWait('jupyter-lab', ADDRESS);
    const retry = await screen.findByRole('button', { name: 'Try Again' }, { timeout: 3000 });
    expect(screen.getByText(/its address could not be loaded/)).toBeVisible();
    expect(mocks.handleError).toHaveBeenCalledTimes(1);
    expect(assign).not.toHaveBeenCalled();

    await userEvent.click(retry);
    await waitFor(() => expect(assign).toHaveBeenCalledWith(`http://master${ADDRESS_WITH_TOKEN}`));
    expect(mocks.getJupyterLab).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('button', { name: 'Try Again' })).not.toBeInTheDocument();
  });

  it('opens a TensorBoard without a token', async () => {
    renderWait('tensor-board', ADDRESS);
    await waitFor(() => expect(assign).toHaveBeenCalled(), { timeout: 3000 });
    expect(assign).toHaveBeenCalledWith(`http://master${ADDRESS}`);
    expect(mocks.getJupyterLab).not.toHaveBeenCalled();
  });
});
