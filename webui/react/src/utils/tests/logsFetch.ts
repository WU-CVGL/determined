export interface LogsRequest {
  signal?: AbortSignal;
  url: string;
}

// A browser's AbortError is an Error, unlike jsdom's DOMException.
const abortError = () =>
  Object.assign(new Error('The operation was aborted.'), { name: 'AbortError' });

/*
 * A fetch for a log viewer's requests that behaves like a browser's: a request whose signal is
 * already aborted is never sent, a follow stream stays open until its signal aborts, and any other
 * log request answers the lines given. `requests` lists the requests that were sent.
 */
export const logsFetch = (
  lines: string[],
): { fetch: (input: string, init?: RequestInit) => Promise<Response>; requests: LogsRequest[] } => {
  const requests: LogsRequest[] = [];
  const body = lines
    .map((message, i) =>
      JSON.stringify({
        result: {
          id: i + 1,
          level: 'LOG_LEVEL_INFO',
          message,
          timestamp: new Date(Date.UTC(2026, 0, 1, 0, 0, i)).toISOString(),
        },
      }),
    )
    .join('\n');

  const fetch = (input: string, init?: RequestInit): Promise<Response> => {
    const signal = init?.signal ?? undefined;
    if (signal?.aborted) return Promise.reject(abortError());
    requests.push({ signal, url: input });

    if (input.includes('/fields')) return Promise.resolve(new Response('', { status: 200 }));
    if (input.includes('follow=true')) {
      return new Promise((_, reject) =>
        signal?.addEventListener('abort', () => reject(abortError())),
      );
    }
    return Promise.resolve(new Response(body, { status: 200 }));
  };

  return { fetch, requests };
};

/* The log requests among the requests given, without the requests of the log fields. */
export const logRequests = (requests: LogsRequest[], follow: boolean): LogsRequest[] =>
  requests.filter((r) => !r.url.includes('/fields') && r.url.includes(`follow=${follow}`));
