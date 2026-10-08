import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import poolAccessChange from 'stores/poolAccessChange';

import SignOut from './SignOut';

const mocks = vi.hoisted(() => ({ logout: vi.fn() }));

vi.mock('services/api', () => ({ logout: mocks.logout }));

describe('SignOut', () => {
  it('ends a pool access change still being sent', async () => {
    mocks.logout.mockResolvedValue(undefined);
    let signal: AbortSignal | undefined;
    poolAccessChange.run('grant', (s) => {
      signal = s;
      return new Promise(() => undefined);
    });
    expect(poolAccessChange.isRunning.get()).toBe(true);

    render(
      <MemoryRouter initialEntries={['/logout']}>
        <Routes>
          <Route element={<SignOut />} path="/logout" />
          <Route element={<div>signed out</div>} path="/login" />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText('signed out')).toBeInTheDocument();
    await waitFor(() => expect(mocks.logout).toHaveBeenCalledTimes(1));
    expect(signal?.aborted).toBe(true);
    expect(poolAccessChange.change.get()).toBeUndefined();
    expect(poolAccessChange.isRunning.get()).toBe(false);
  });
});
