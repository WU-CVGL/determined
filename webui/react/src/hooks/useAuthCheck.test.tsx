import { act, render, renderHook, waitFor } from '@testing-library/react';
import { WritableObservable } from 'micro-observables';
import React, { useEffect } from 'react';
import { createMemoryRouter, MemoryRouter, RouterProvider, useLocation } from 'react-router-dom';

import { getCurrentUser, storeSessionToken } from 'services/api';
import authStore from 'stores/auth';
import determinedStore, { DeterminedInfo } from 'stores/determinedInfo';
import userStore from 'stores/users';
import { reloadPage } from 'utils/browser';
import { DetError, ErrorType } from 'utils/error';
import { routeToExternalUrl } from 'utils/routes';

import useAuthCheck from './useAuthCheck';

const { routeAll } = vi.hoisted(() => ({ routeAll: vi.fn() }));

vi.mock('services/api', () => ({
  getCurrentUser: vi.fn(),
  storeSessionToken: vi.fn(),
}));

vi.mock('stores/determinedInfo', async () => {
  const { observable } = await import('micro-observables');
  return { default: { info: observable({}) } };
});

const setInfo = (info: Partial<DeterminedInfo>) =>
  (determinedStore.info as unknown as WritableObservable<Partial<DeterminedInfo>>).set(info);

vi.mock('routes/utils', () => ({
  paths: { login: () => '/login', logout: () => '/logout' },
  routeAll,
}));

vi.mock('utils/routes', () => ({ routeToExternalUrl: vi.fn() }));

vi.mock('utils/browser', () => ({ reloadPage: vi.fn() }));

const SIGNED_OUT = () => new DetError(new Response(null, { status: 401 }));

const USER = { id: 1, isActive: true, isAdmin: false, username: 'u' };

/* The script that removes a session cookie scripts can read, the only cookie the web UI writes. */
const REMOVE_READABLE_COOKIE = 'auth=; Max-Age=0; path=/';

/* jsdom's cookie jar, which holds the browser's cookies, HttpOnly ones included. */
interface CookieJar {
  getCookieStringSync(url: string): string;
  removeAllCookiesSync(): void;
  setCookieSync(cookie: string, url: string): unknown;
}
const cookieJar = (): CookieJar =>
  (globalThis as unknown as { jsdom: { cookieJar: CookieJar } }).jsdom.cookieJar;

/* Sets a cookie the way a response from the master does, so that it can be HttpOnly. */
const setCookieFromMaster = (cookie: string) =>
  cookieJar().setCookieSync(cookie, window.location.href);

/* The master's /auth/session-cookie: it answers later, storing the token in the HttpOnly cookie. */
const masterStoresToken = async ({ token }: { token: string }) => {
  await new Promise((resolve) => setTimeout(resolve));
  setCookieFromMaster(`auth=${token}; Path=/; HttpOnly; SameSite=Lax`);
};

const setup = (url: string) => {
  // The browser's address bar, which the token must leave, and the router's own location.
  window.history.replaceState(null, '', url);
  const wrapper = ({ children }: React.PropsWithChildren) => (
    <MemoryRouter initialEntries={[url]}>{children}</MemoryRouter>
  );
  return renderHook(() => ({ check: useAuthCheck(), location: useLocation() }), { wrapper });
};

const check = async (result: ReturnType<typeof setup>['result']): Promise<boolean> => {
  let signedIn = false;
  await act(async () => {
    signedIn = await result.current.check();
  });
  return signedIn;
};

describe('useAuthCheck', () => {
  let cookieWrites: string[];

  beforeEach(() => {
    setInfo({ ssoProviders: [] });
    authStore.reset();
    userStore.reset();
    vi.mocked(getCurrentUser).mockReset();
    vi.mocked(storeSessionToken).mockReset().mockResolvedValue(undefined);
    vi.mocked(routeToExternalUrl).mockReset();
    routeAll.mockReset();
    vi.mocked(reloadPage).mockReset();
    window.localStorage.clear();
    cookieJar().removeAllCookiesSync();

    // The web UI must never write a token into a cookie: the session cookie is the master's and
    // HttpOnly.
    cookieWrites = [];
    const descriptor = Object.getOwnPropertyDescriptor(Document.prototype, 'cookie');
    vi.spyOn(document, 'cookie', 'set').mockImplementation((value: string) => {
      cookieWrites.push(value);
      descriptor?.set?.call(document, value);
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState(null, '', '/');
    // Nothing in the web UI keeps a session token where scripts can read it. The only cookie it
    // writes removes a session cookie that an earlier version left readable.
    expect(cookieWrites.filter((c) => c !== REMOVE_READABLE_COOKIE)).toStrictEqual([]);
    expect(document.cookie).not.toMatch(/(^|; )auth=/);
    expect(Object.keys(window.localStorage).filter((k) => /auth|token/i.test(k))).toStrictEqual([]);
    expect(Object.keys(window.sessionStorage).filter((k) => /auth|token/i.test(k))).toStrictEqual(
      [],
    );
  });

  it('signs in with only the session cookie', async () => {
    vi.mocked(getCurrentUser).mockResolvedValue({
      id: 1,
      isActive: true,
      isAdmin: false,
      username: 'u',
    });
    const { result } = setup('/det/login');

    expect(await check(result)).toBe(true);
    expect(getCurrentUser).toHaveBeenCalledTimes(1);
    expect(storeSessionToken).not.toHaveBeenCalled();
    expect(authStore.isAuthenticated.get()).toBe(true);
    expect(authStore.isChecked.get()).toBe(true);
  });

  it('finds the browser signed out when the master refuses the cookie', async () => {
    vi.mocked(getCurrentUser).mockRejectedValue(SIGNED_OUT());
    const { result } = setup('/det/login');

    expect(await check(result)).toBe(false);
    expect(authStore.isAuthenticated.get()).toBe(false);
    expect(authStore.isChecked.get()).toBe(true);
    expect(routeToExternalUrl).not.toHaveBeenCalled();
    expect(routeAll).not.toHaveBeenCalled();
  });

  it('takes a token from the external sign-in page out of the URL and stores it as the cookie', async () => {
    setInfo({ externalLoginUri: 'https://login.example/', ssoProviders: [] });
    vi.mocked(getCurrentUser).mockResolvedValue({
      id: 1,
      isActive: true,
      isAdmin: false,
      username: 'u',
    });
    const { result } = setup('/det/login?jwt=v2.public.tok&redirect=%2Fdet%2Fmodels');
    const replaceState = vi.spyOn(window.history, 'replaceState');

    expect(await check(result)).toBe(true);
    expect(storeSessionToken).toHaveBeenCalledTimes(1);
    expect(vi.mocked(storeSessionToken).mock.calls[0][0]).toStrictEqual({ token: 'v2.public.tok' });
    // The token is gone from the browser's history before it is sent anywhere, and from the
    // router's location; the other parameters stay.
    expect(window.location.pathname).toBe('/det/login');
    expect(new URLSearchParams(window.location.search).has('jwt')).toBe(false);
    expect(new URLSearchParams(window.location.search).get('redirect')).toBe('/det/models');
    expect(replaceState.mock.invocationCallOrder[0]).toBeLessThan(
      vi.mocked(storeSessionToken).mock.invocationCallOrder[0],
    );
    const params = new URLSearchParams(result.current.location.search);
    expect(params.has('jwt')).toBe(false);
    expect(params.get('redirect')).toBe('/det/models');
    // The cookie is checked only after the master has stored it.
    expect(vi.mocked(storeSessionToken).mock.invocationCallOrder[0]).toBeLessThan(
      vi.mocked(getCurrentUser).mock.invocationCallOrder[0],
    );
    expect(authStore.isAuthenticated.get()).toBe(true);
  });

  it('ignores a token in the URL when no external sign-in page is configured', async () => {
    vi.mocked(getCurrentUser).mockRejectedValue(SIGNED_OUT());
    const { result } = setup('/det/login?jwt=v2.public.attacker');

    expect(await check(result)).toBe(false);
    // Anyone could send such a link, to sign visitors in to the link author's account.
    expect(storeSessionToken).not.toHaveBeenCalled();
    expect(new URLSearchParams(window.location.search).has('jwt')).toBe(false);
    expect(new URLSearchParams(result.current.location.search).has('jwt')).toBe(false);
    expect(authStore.isAuthenticated.get()).toBe(false);
  });

  it('checks the cookie when the master refuses the token from the URL', async () => {
    setInfo({ externalLoginUri: 'https://login.example/', ssoProviders: [] });
    vi.mocked(storeSessionToken).mockRejectedValue(SIGNED_OUT());
    vi.mocked(getCurrentUser).mockRejectedValue(SIGNED_OUT());
    const { result } = setup('/det/login?jwt=expired');

    expect(await check(result)).toBe(false);
    expect(authStore.isAuthenticated.get()).toBe(false);
    // Signed out with an external sign-in page: go there.
    expect(routeAll).toHaveBeenCalledWith(
      expect.stringMatching(/^https:\/\/login\.example\/\?redirect=/),
    );
  });

  it('stays signed in with the cookie when an SSO provider always redirects', async () => {
    setInfo({
      ssoProviders: [
        { alwaysRedirect: true, name: 'okta', ssoUrl: 'https://sso/oidc', type: 'OIDC' },
      ],
    });
    vi.mocked(getCurrentUser).mockResolvedValue({
      id: 1,
      isActive: true,
      isAdmin: false,
      username: 'u',
    });
    const { result } = setup('/det/login');

    expect(await check(result)).toBe(true);
    expect(authStore.isAuthenticated.get()).toBe(true);
    expect(authStore.isChecked.get()).toBe(true);
    expect(routeToExternalUrl).not.toHaveBeenCalled();
  });

  it('sends signed-out visitors to an SSO provider that always redirects', async () => {
    setInfo({
      ssoProviders: [
        { alwaysRedirect: true, name: 'okta', ssoUrl: 'https://sso/oidc', type: 'OIDC' },
      ],
    });
    vi.mocked(getCurrentUser).mockRejectedValue(SIGNED_OUT());
    const { result } = setup('/det/login');

    expect(await check(result)).toBe(false);
    expect(routeToExternalUrl).toHaveBeenCalledWith('https://sso/oidc');
  });

  it('sends users to SSO again when their remote session expired', async () => {
    setInfo({
      ssoProviders: [{ name: 'okta', ssoUrl: 'https://sso/oidc', type: 'OIDC' }],
    });
    vi.mocked(getCurrentUser).mockRejectedValue(
      new DetError(undefined, { publicMessage: 'remote user token expired', type: ErrorType.Auth }),
    );
    const { result } = setup('/det/login');

    expect(await check(result)).toBe(false);
    expect(routeToExternalUrl).toHaveBeenCalledWith('https://sso/oidc');
    expect(authStore.isAuthenticated.get()).toBe(false);
  });

  it('checks the session once, not again on every navigation', async () => {
    vi.mocked(getCurrentUser).mockResolvedValue({
      id: 1,
      isActive: true,
      isAdmin: false,
      username: 'u',
    });
    // Like App, which checks the session whenever checkAuth changes.
    const AppLike = () => {
      const checkAuth = useAuthCheck();
      useEffect(() => {
        checkAuth();
      }, [checkAuth]);
      return null;
    };
    const router = createMemoryRouter([{ element: <AppLike />, path: '*' }], {
      initialEntries: ['/det/dashboard'],
    });
    render(<RouterProvider router={router} />);
    await waitFor(() => expect(authStore.isAuthenticated.get()).toBe(true));

    for (const path of ['/det/models', '/det/experiments', '/det/workspaces']) {
      await act(() => router.navigate(path));
    }
    await act(() => Promise.resolve());

    expect(router.state.location.pathname).toBe('/det/workspaces');
    expect(getCurrentUser).toHaveBeenCalledTimes(1);
  });

  it('reloads the page when another tab signed in as someone else', async () => {
    userStore.updateCurrentUser(USER);
    vi.mocked(getCurrentUser).mockResolvedValue({ ...USER, id: 2, username: 'other' });
    const { result } = setup('/det/models');

    expect(await check(result)).toBe(true);
    // This tab's requests now run as the other user, so it must not keep showing the one it loaded.
    expect(reloadPage).toHaveBeenCalledTimes(1);
  });

  it('does not reload the page while the same user is signed in', async () => {
    userStore.updateCurrentUser(USER);
    vi.mocked(getCurrentUser).mockResolvedValue({ ...USER, username: 'renamed' });
    const { result } = setup('/det/models');

    expect(await check(result)).toBe(true);
    expect(reloadPage).not.toHaveBeenCalled();
  });

  it('does not reload the page before it has loaded its user', async () => {
    vi.mocked(getCurrentUser).mockResolvedValue({ ...USER, id: 2, username: 'other' });
    const { result } = setup('/det/models');

    expect(await check(result)).toBe(true);
    expect(reloadPage).not.toHaveBeenCalled();
  });

  it('replaces a session cookie that scripts can read with the HttpOnly one', async () => {
    // Earlier versions of the master and the web UI set the session cookie without HttpOnly.
    setCookieFromMaster('auth=v2.public.legacy; Path=/');
    vi.mocked(storeSessionToken).mockImplementation(masterStoresToken);
    vi.mocked(getCurrentUser).mockResolvedValue(USER);
    const { result } = setup('/det/models');
    expect(document.cookie).toBe('auth=v2.public.legacy');

    expect(await check(result)).toBe(true);
    // The master stored the same token in the HttpOnly cookie, which replaced the readable one.
    expect(vi.mocked(storeSessionToken).mock.calls).toStrictEqual([
      [{ token: 'v2.public.legacy' }],
    ]);
    expect(document.cookie).toBe('');
    expect(cookieJar().getCookieStringSync(window.location.href)).toBe('auth=v2.public.legacy');
    expect(cookieWrites).toStrictEqual([]);
    // The session is checked only once the cookie is HttpOnly.
    expect(vi.mocked(storeSessionToken).mock.invocationCallOrder[0]).toBeLessThan(
      vi.mocked(getCurrentUser).mock.invocationCallOrder[0],
    );
    expect(authStore.isAuthenticated.get()).toBe(true);
  });

  it('removes a session cookie that scripts can read when the master refuses its token', async () => {
    setCookieFromMaster('auth=v2.public.ended; Path=/');
    vi.mocked(storeSessionToken).mockRejectedValue(SIGNED_OUT());
    vi.mocked(getCurrentUser).mockRejectedValue(SIGNED_OUT());
    const { result } = setup('/det/models');

    expect(await check(result)).toBe(false);
    expect(vi.mocked(storeSessionToken).mock.calls).toStrictEqual([[{ token: 'v2.public.ended' }]]);
    expect(cookieWrites).toStrictEqual([REMOVE_READABLE_COOKIE]);
    expect(document.cookie).toBe('');
    expect(cookieJar().getCookieStringSync(window.location.href)).toBe('');
    expect(vi.mocked(storeSessionToken).mock.invocationCallOrder[0]).toBeLessThan(
      vi.mocked(getCurrentUser).mock.invocationCallOrder[0],
    );
    expect(authStore.isAuthenticated.get()).toBe(false);
  });

  it('leaves other cookies alone and sends nothing when no session cookie is readable', async () => {
    setCookieFromMaster('auth=v2.public.current; Path=/; HttpOnly');
    setCookieFromMaster('authority=x; Path=/');
    setCookieFromMaster('xauth=y; Path=/');
    vi.mocked(getCurrentUser).mockResolvedValue(USER);
    const { result } = setup('/det/models');

    expect(await check(result)).toBe(true);
    expect(storeSessionToken).not.toHaveBeenCalled();
    expect(cookieWrites).toStrictEqual([]);
    expect(document.cookie).toBe('authority=x; xauth=y');
  });

  it('sends the token of a readable session cookie once when checks overlap', async () => {
    setCookieFromMaster('auth=v2.public.legacy; Path=/');
    vi.mocked(storeSessionToken).mockImplementation(masterStoresToken);
    vi.mocked(getCurrentUser).mockResolvedValue(USER);
    const { result } = setup('/det/models');

    await act(async () => {
      await Promise.all([result.current.check(), result.current.check()]);
    });
    expect(storeSessionToken).toHaveBeenCalledTimes(1);
    expect(getCurrentUser).toHaveBeenCalledTimes(2);
    expect(document.cookie).toBe('');
  });

  it('stores a token from the URL before looking for a readable session cookie', async () => {
    setInfo({ externalLoginUri: 'https://login.example/', ssoProviders: [] });
    setCookieFromMaster('auth=v2.public.legacy; Path=/');
    vi.mocked(storeSessionToken).mockImplementation(masterStoresToken);
    vi.mocked(getCurrentUser).mockResolvedValue(USER);
    const { result } = setup('/det/login?jwt=v2.public.tok');

    expect(await check(result)).toBe(true);
    // The cookie from the URL's token replaced the readable one; the old token is not sent.
    expect(vi.mocked(storeSessionToken).mock.calls).toStrictEqual([[{ token: 'v2.public.tok' }]]);
    expect(cookieJar().getCookieStringSync(window.location.href)).toBe('auth=v2.public.tok');
  });

  it('keeps what it knew when the master cannot be reached', async () => {
    authStore.setAuth({ isAuthenticated: true });
    vi.mocked(getCurrentUser).mockRejectedValue(new DetError(new TypeError('Failed to fetch')));
    const { result } = setup('/det/models');

    expect(await check(result)).toBe(true);
    expect(authStore.isAuthenticated.get()).toBe(true);
    expect(authStore.isChecked.get()).toBe(true);
  });
});
