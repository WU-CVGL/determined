import { render } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { detApi } from 'services/apiConfig';
import { readStream } from 'services/utils';
import { RunState, TrialDetails } from 'types';

import TrialLogPreview from './TrialLogPreview';

vi.mock('services/apiConfig', () => ({
  detApi: { StreamingExperiments: { trialLogs: vi.fn(() => ({ options: {}, url: '' })) } },
}));
vi.mock('services/utils', () => ({ readStream: vi.fn() }));

const trial = { id: 7, state: RunState.Running } as TrialDetails;

const setup = (logsHidden: boolean) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <TrialLogPreview logsHidden={logsHidden} trial={trial} />
    </UIProvider>,
  );

describe('TrialLogPreview', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('opens no stream for logs that are not shared', () => {
    setup(true);
    expect(detApi.StreamingExperiments.trialLogs).not.toHaveBeenCalled();
    expect(readStream).not.toHaveBeenCalled();
  });

  it('reads the latest log line of a running trial', () => {
    setup(false);
    expect(detApi.StreamingExperiments.trialLogs).toHaveBeenCalledTimes(1);
    expect(readStream).toHaveBeenCalledTimes(1);
  });
});
