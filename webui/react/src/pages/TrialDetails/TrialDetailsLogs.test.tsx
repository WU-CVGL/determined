import { act, render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';

import { ThemeProvider } from 'components/ThemeProvider';
import usePageVisibility from 'hooks/usePageVisibility';
import { detApi } from 'services/apiConfig';
import { generateTestExperimentData } from 'utils/tests/generateTestData';
import { logRequests, logsFetch } from 'utils/tests/logsFetch';

import TrialDetailsLogs from './TrialDetailsLogs';

vi.mock('services/apiConfig', () => ({
  detApi: {
    StreamingExperiments: {
      // The request options are the last argument, with the signal of the request.
      trialLogs: vi.fn((id: number, limit: number, follow: boolean, ...rest: unknown[]) => ({
        options: rest.at(-1),
        url: `/api/v1/trials/${id}/logs?limit=${limit}&follow=${follow}`,
      })),
      trialLogsFields: vi.fn((id: number, follow: boolean, options: unknown) => ({
        options,
        url: `/api/v1/trials/${id}/logs/fields?follow=${follow}`,
      })),
    },
  },
}));
vi.mock('hooks/useSettings', () => {
  // The same settings on every render, as the settings store gives them.
  const value = { resetSettings: vi.fn(), settings: {}, updateSettings: vi.fn() };
  return { useSettings: () => value };
});
// Renders every log line: jsdom gives the viewer's scroll parent no height, so Virtuoso shows none.
vi.mock('react-virtuoso', async (importOriginal) => {
  const { forwardRef, Fragment, useImperativeHandle } =
    await vi.importActual<typeof import('react')>('react');
  const List = forwardRef<
    { scrollToIndex: () => void },
    { data?: unknown[]; itemContent: (index: number, item: unknown) => React.ReactNode }
  >(({ data, itemContent }, ref) => {
    useImperativeHandle(ref, () => ({ scrollToIndex: () => undefined }));
    return (
      <div>
        {data?.map((item, index) => <Fragment key={index}>{itemContent(index, item)}</Fragment>)}
      </div>
    );
  });
  return { ...(await importOriginal<typeof import('react-virtuoso')>()), Virtuoso: List };
});
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => false }) }));

const { experiment, trial } = generateTestExperimentData();

const capabilities = (viewLogs: boolean) => ({
  manageSharing: false,
  viewConfig: true,
  viewLogs,
  viewTitle: true,
});

const PageVisibility = () => {
  usePageVisibility();
  return null;
};

const setPageHidden = (hidden: boolean) =>
  act(() => {
    Object.defineProperty(document, 'hidden', { configurable: true, value: hidden });
    document.dispatchEvent(new Event('visibilitychange'));
  });

const logs = (viewLogs: boolean) => (
  <UIProvider theme={DefaultTheme.Light}>
    <ThemeProvider>
      <PageVisibility />
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

  describe('when the logs are hidden and shared again without a refusal', () => {
    afterEach(() => {
      vi.unstubAllGlobals();
      Object.defineProperty(document, 'hidden', { configurable: true, value: false });
    });

    it('keeps following the logs when the user leaves the page and comes back', async () => {
      const { fetch, requests } = logsFetch(['first line', 'second line']);
      vi.stubGlobal('fetch', fetch);
      render(logs(true));
      expect(await screen.findByText('second line')).toBeInTheDocument();
      await waitFor(() => expect(logRequests(requests, true)).toHaveLength(1));

      setPageHidden(true);
      setPageHidden(false);
      await settle();
      expect(screen.getByText('second line')).toBeInTheDocument();
      expect(logRequests(requests, true).filter((r) => !r.signal?.aborted)).toHaveLength(1);
    });

    it('reads the logs again and follows them', async () => {
      const { fetch, requests } = logsFetch(['first line', 'second line']);
      vi.stubGlobal('fetch', fetch);
      const view = render(logs(true));
      expect(await screen.findByText('second line')).toBeInTheDocument();
      await waitFor(() => expect(logRequests(requests, true)).toHaveLength(1));

      view.rerender(logs(false));
      expect(screen.getByText('Logs not shared')).toBeInTheDocument();
      expect(requests.every((r) => r.signal?.aborted)).toBe(true);
      const sent = requests.length;

      view.rerender(logs(true));
      expect(await screen.findByText('second line')).toBeInTheDocument();
      expect(screen.getByText('first line')).toBeInTheDocument();
      expect(screen.queryByText('No logs to show.')).not.toBeInTheDocument();
      const after = requests.slice(sent);
      expect(logRequests(after, false)).toHaveLength(1);
      expect(logRequests(after, true)).toHaveLength(1);
      expect(logRequests(after, true)[0].signal?.aborted).toBe(false);
    });
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
