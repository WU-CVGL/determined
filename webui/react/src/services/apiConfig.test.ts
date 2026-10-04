import * as utils from './apiConfig';

describe('apiConfig', () => {
  describe('getUserIds', () => {
    it('should convert user id strings into user id numbers', () => {
      expect(utils.getUserIds(['123'])).toStrictEqual([123]);
      expect(utils.getUserIds(['456', '789'])).toStrictEqual([456, 789]);
    });

    it('should filter out non-numeric string user ids', () => {
      expect(utils.getUserIds(['abc', '123'])).toStrictEqual([123]);
    });

    it('should return `undefined` when there are no valid user ids', () => {
      expect(utils.getUserIds(['abc', 'def'])).toBeUndefined();
    });
  });
});

describe('requests to the master', () => {
  const okResponse = () =>
    new Response(JSON.stringify({ user: { id: 3, username: 'u3' } }), {
      headers: { 'Content-Type': 'application/json' },
      status: 200,
    });
  let fetchMock: ReturnType<typeof vi.fn<[RequestInfo | URL, RequestInit?], Promise<Response>>>;

  beforeEach(() => {
    fetchMock = vi.fn<[RequestInfo | URL, RequestInit?], Promise<Response>>(() =>
      Promise.resolve(okResponse()),
    );
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const sent = (): { headers: Record<string, string>; init: RequestInit; url: string } => {
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    return { headers: (init.headers ?? {}) as Record<string, string>, init, url };
  };

  it('sends no token: the session cookie authenticates the web UI', async () => {
    await utils.getCurrentUser.request({});
    const { headers, url } = sent();
    expect(url).toMatch(/\/api\/v1\/auth\/user$/);
    expect(headers).not.toHaveProperty('Authorization');
  });

  it('asks the master to keep a token from the URL as the session cookie', async () => {
    fetchMock.mockImplementation(() => Promise.resolve(new Response(null, { status: 204 })));
    await utils.storeSessionToken.request({ token: 'v2.public.tok' });
    const { headers, init, url } = sent();
    expect(url).toMatch(/\/auth\/session-cookie$/);
    expect(init.method).toBe('POST');
    expect(headers).toStrictEqual({ Authorization: 'Bearer v2.public.tok' });

    fetchMock.mockImplementation(() => Promise.resolve(new Response(null, { status: 401 })));
    await expect(utils.storeSessionToken.request({ token: 'bad' })).rejects.toBeInstanceOf(
      Response,
    );
  });
});
