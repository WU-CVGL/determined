import { act, render, screen, waitFor } from '@testing-library/react';
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

const capabilities = (viewLogs: boolean) => ({
  manageSharing: false,
  viewConfig: true,
  viewLogs,
  viewTitle: true,
});

const logs = (viewLogs: boolean) => (
  <UIProvider theme={DefaultTheme.Light}>
    <ThemeProvider>
      <ConfirmationProvider>
        <TrialDetailsLogs
          experiment={{ ...experiment, capabilities: capabilities(viewLogs) }}
          trial={trial}
        />
      </ConfirmationProvider>
    </ThemeProvider>
  </UIProvider>
);

// The master refuses the logs of a job whose logs are not shared.
const refusal = () =>
  new Response(JSON.stringify({ error: { httpCode: 403, message: 'logs not shared' } }), {
    status: 403,
  });

// Gives a request the time to start.
const settle = () => act(() => new Promise((resolve) => setTimeout(resolve, 300)));

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

  describe('after a refusal of the master', () => {
    const fetchMock = vi.fn();

    beforeEach(() => {
      vi.mocked(detApi.StreamingExperiments.trialLogsFields).mockClear();
      fetchMock.mockReset().mockImplementation(() => Promise.resolve(refusal()));
      vi.stubGlobal('fetch', fetchMock);
    });

    afterEach(() => {
      vi.unstubAllGlobals();
    });

    it('opens the logs again when the capabilities show them again', async () => {
      const view = render(logs(true));
      expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
      view.rerender(logs(false));
      const opened = vi.mocked(detApi.StreamingExperiments.trialLogsFields).mock.calls.length;

      fetchMock.mockImplementation(() => Promise.resolve(new Response('', { status: 200 })));
      view.rerender(logs(true));
      await waitFor(() => expect(screen.queryByText('Logs not shared')).not.toBeInTheDocument());
      expect(detApi.StreamingExperiments.trialLogsFields).toHaveBeenCalledTimes(opened + 1);
    });

    it('keeps the refusal while stale capabilities still show the logs', async () => {
      const view = render(logs(true));
      expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
      await settle();
      const calls = fetchMock.mock.calls.length;

      view.rerender(logs(true));
      view.rerender(logs(true));
      await settle();
      expect(screen.getByText('Logs not shared')).toBeInTheDocument();
      expect(fetchMock.mock.calls.length).toBe(calls);
    });
  });
});
