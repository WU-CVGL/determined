import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import Button from 'hew/Button';
import { useModal } from 'hew/Modal';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import React from 'react';

import { CHANGE_RUNNING_MESSAGE, changeStoppedMessage } from 'components/PoolAccessResults';
import { ThemeProvider } from 'components/ThemeProvider';
import { PoolAccessResult } from 'utils/resourcePoolAccess';

import PoolAccessConfirmModalComponent from './PoolAccessConfirmModal';

const OPEN = 'Open';

// Modal tests render antd components, which is slow when the whole suite runs.
vi.setConfig({ testTimeout: 15_000 });

type Run = () => Promise<PoolAccessResult[] | undefined>;

const Container: React.FC<{ run: Run }> = ({ run }) => {
  const ConfirmModal = useModal(PoolAccessConfirmModalComponent);
  return (
    <div>
      <Button onClick={ConfirmModal.open}>{OPEN}</Button>
      <ConfirmModal.Component
        action="restrict"
        closeModal={() => ConfirmModal.close('cancel')}
        content={<p>Restrict cpu?</p>}
        okText="Restrict 1 pool"
        run={run}
        title="Restrict 1 pool"
      />
    </div>
  );
};

const user = userEvent.setup();

const setup = async (run: Run) => {
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <Container run={run} />
      </ThemeProvider>
    </UIProvider>,
  );
  await user.click(screen.getByText(OPEN));
  await user.click(await screen.findByRole('button', { name: 'Restrict 1 pool' }));
};

describe('PoolAccessConfirmModal', () => {
  it('says that another change runs when its change is refused', async () => {
    const run = vi.fn<[], ReturnType<Run>>(() => Promise.resolve(undefined));
    await setup(run);

    expect(await screen.findByText(CHANGE_RUNNING_MESSAGE)).toBeInTheDocument();
    expect(run).toHaveBeenCalledTimes(1);
    expect(screen.getByText('Restrict cpu?')).toBeInTheDocument();
    expect(screen.queryByTestId('pool-access-results')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Restrict 1 pool' })).toBeEnabled();
  });

  it('says why a change stopped when it throws', async () => {
    await setup(() => Promise.reject(new Error('bug')));

    expect(await screen.findByText(changeStoppedMessage('bug'))).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Restrict 1 pool' })).toBeEnabled();
  });
});
