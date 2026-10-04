import { act, renderHook } from '@testing-library/react';
import { WritableObservable } from 'micro-observables';
import React from 'react';
import { MemoryRouter, useLocation } from 'react-router-dom';

import { getCurrentUser, storeSessionToken } from 'services/api';
import authStore from 'stores/auth';
import determinedStore, { DeterminedInfo } from 'stores/determinedInfo';
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

const SIGNED_OUT = () => new DetError(new Response(null, { status: 401 }));

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
    vi.mocked(getCurrentUser).mockReset();
    vi.mocked(storeSessionToken).mockReset().mockResolvedValue(undefined);
    vi.mocked(routeToExternalUrl).mockReset();
    routeAll.mockReset();
    window.localStorage.clear();

    // The web UI must never write a cookie: the session cookie is the master's and HttpOnly.
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
    // Nothing in the web UI keeps a session token where scripts can read it.
    expect(cookieWrites).toStrictEqual([]);
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

  it('keeps what it knew when the master cannot be reached', async () => {
    authStore.setAuth({ isAuthenticated: true });
    vi.mocked(getCurrentUser).mockRejectedValue(new DetError(new TypeError('Failed to fetch')));
    const { result } = setup('/det/models');

    expect(await check(result)).toBe(true);
    expect(authStore.isAuthenticated.get()).toBe(true);
    expect(authStore.isChecked.get()).toBe(true);
  });
});
