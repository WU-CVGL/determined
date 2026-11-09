import { render, screen } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';

import { ThemeProvider } from 'components/ThemeProvider';
import { detApi } from 'services/apiConfig';
import { generateTestExperimentData } from 'utils/tests/generateTestData';

import TrialDetailsLogs from './TrialDetailsLogs';

vi.mock('services/apiConfig', () => ({
  detApi: {
    StreamingExperiments: {
      trialLogs: vi.fn(() => ({ options: {}, url: '/api/v1/trials/1/logs' })),
      trialLogsFields: vi.fn(() => ({ options: {}, url: '/api/v1/trials/1/logs/fields' })),
    },
  },
}));
vi.mock('hooks/useSettings', () => ({
  useSettings: () => ({ resetSettings: vi.fn(), settings: {}, updateSettings: vi.fn() }),
}));
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => false }) }));

const { experiment, trial } = generateTestExperimentData();

describe('TrialDetailsLogs', () => {
  it('opens no stream for logs that are not shared', () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    render(
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <ConfirmationProvider>
            <TrialDetailsLogs
              experiment={{
                ...experiment,
                capabilities: {
                  manageSharing: false,
                  viewConfig: true,
                  viewLogs: false,
                  viewTitle: true,
                },
              }}
              trial={trial}
            />
          </ConfirmationProvider>
        </ThemeProvider>
      </UIProvider>,
    );
    expect(screen.getByText('Logs not shared')).toBeInTheDocument();
    expect(detApi.StreamingExperiments.trialLogs).not.toHaveBeenCalled();
    expect(detApi.StreamingExperiments.trialLogsFields).not.toHaveBeenCalled();
    expect(fetchMock).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });
});
