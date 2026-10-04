import { Loadable } from 'hew/utils/loadable';
import { useObservable } from 'micro-observables';
import { useCallback, useInsertionEffect, useRef } from 'react';
import { useSearchParams } from 'react-router-dom';

import { samlUrl } from 'ee/SamlAuth';
import { paths, routeAll } from 'routes/utils';
import { getCurrentUser, storeSessionToken } from 'services/api';
import authStore from 'stores/auth';
import determinedStore from 'stores/determinedInfo';
import userStore from 'stores/users';
import { reloadPage } from 'utils/browser';
import handleError from 'utils/error';
import { routeToExternalUrl } from 'utils/routes';
import { isAuthFailure, isRemoteUserTokenExpired } from 'utils/service';

export const JWT_PARAM = 'jwt';

/*
 * The exchange of a token from the URL for the session cookie, while it runs. Auth checks that
 * start meanwhile (removing the token from the URL starts one) wait for it, so that none of them
 * finds the browser signed out just before the cookie arrives.
 */
let pendingTokenExchange: Promise<void> | undefined;

/* The name of the master's session cookie. */
const SESSION_COOKIE_NAME = 'auth';

/*
 * Earlier versions of the master and the web UI kept the session token in a session cookie that
 * scripts could read. The master now sets the cookie HttpOnly, so a session cookie that shows in
 * document.cookie is one of those, and any script on the master's origin can steal its token.
 */
const readableSessionToken = (): string | undefined => {
  const prefix = `${SESSION_COOKIE_NAME}=`;
  return document.cookie
    .split(';')
    .map((cookie) => cookie.trim())
    .find((cookie) => cookie.startsWith(prefix))
    ?.slice(prefix.length);
};

/*
 * The replacement of a session cookie that scripts can read, while it runs. Auth checks that start
 * meanwhile wait for it instead of sending the token again.
 */
let pendingCookieReplacement: Promise<void> | undefined;

/*
 * Has the master store the token of a session cookie that scripts can read in its HttpOnly cookie,
 * which has the same name and path and so replaces it. A readable cookie still there afterwards
 * (its session has ended, or the master refused or could not be reached) is removed, and the user
 * signs in again. Nothing here writes the token anywhere.
 */
const replaceReadableSessionCookie = async (): Promise<void> => {
  const token = readableSessionToken();
  if (token === undefined) return;
  if (token) {
    try {
      await storeSessionToken({ token });
    } catch (e) {
      if (!isAuthFailure(e)) handleError(e, { silent: true });
    }
  }
  if (readableSessionToken() !== undefined) {
    document.cookie = `${SESSION_COOKIE_NAME}=; Max-Age=0; path=/`;
  }
};

/* Replaces the browser's history entry with its URL without the token, keeping the router's state. */
const removeTokenFromHistory = (): void => {
  const url = new URL(window.location.href);
  if (!url.searchParams.has(JWT_PARAM)) return;
  url.searchParams.delete(JWT_PARAM);
  window.history.replaceState(window.history.state, '', url.href);
};

/**
 * Returns a function that finds out whether the browser is signed in, and records it in authStore.
 *
 * The browser's session is the master's HttpOnly session cookie, which the web UI cannot read, so
 * this asks the master who the current user is. The external sign-in page (`externalLoginUri`)
 * hands over a token in the URL instead (`?jwt=`); the master turns it into the cookie. A session
 * cookie that an earlier version left readable by scripts is replaced before the check.
 */
const useAuthCheck = (): (() => Promise<boolean>) => {
  const info = useObservable(determinedStore.info);
  const [searchParams, setSearchParams] = useSearchParams();
  // setSearchParams changes on every navigation, and App checks the session again whenever
  // checkAuth changes, so checkAuth reaches it through a ref instead of depending on it.
  const setSearchParamsRef = useRef(setSearchParams);
  useInsertionEffect(() => {
    setSearchParamsRef.current = setSearchParams;
  }, [setSearchParams]);

  const redirectToExternalSignin = useCallback(() => {
    const { pathname: path, origin, href } = window.location;
    const redirect = [paths.login(), paths.logout()].some((p) => path.includes(p))
      ? origin
      : encodeURIComponent(href);
    const authUrl = `${info.externalLoginUri}?redirect=${redirect}`;
    routeAll(authUrl);
  }, [info.externalLoginUri]);

  const redirectToSSO = useCallback(() => {
    info.ssoProviders?.forEach((ssoProvider) => {
      if (ssoProvider.type === 'OIDC') {
        routeToExternalUrl(ssoProvider.ssoUrl);
      } else {
        routeToExternalUrl(samlUrl(ssoProvider.ssoUrl));
      }
    });
  }, [info.ssoProviders]);

  const checkAuth = useCallback(async (): Promise<boolean> => {
    if (searchParams.has(JWT_PARAM)) {
      // Take the token out of the URL before anything else, so that it does not stay in the
      // browser's history or reach other pages in a Referer header. The router changes the
      // history only once its navigation runs, so replace the entry first, then tell the router.
      removeTokenFromHistory();
      const jwt = searchParams.getAll(JWT_PARAM);
      const rest = new URLSearchParams(searchParams);
      rest.delete(JWT_PARAM);
      setSearchParamsRef.current(rest, { replace: true });

      // Only the external sign-in page sends a token. Accepting one from any link would let the
      // link's author sign visitors in to an account of their choosing.
      if (info.externalLoginUri && jwt.length === 1) {
        pendingTokenExchange = storeSessionToken({ token: jwt[0] }).finally(() => {
          pendingTokenExchange = undefined;
        });
      }
    }
    if (pendingTokenExchange) {
      try {
        await pendingTokenExchange;
      } catch (e) {
        // The cookie check below finds out whether the browser is signed in.
        if (!isAuthFailure(e)) handleError(e, { silent: true });
      }
    }
    // Only after a token from the URL is stored, so that the two do not race for the cookie.
    pendingCookieReplacement ??= replaceReadableSessionCookie().finally(() => {
      pendingCookieReplacement = undefined;
    });
    await pendingCookieReplacement;

    // check if the user clicked the logout button, which ignores SSO redirection
    const hardLogout = window.location.href.includes('hard_logout=true');

    try {
      const user = await getCurrentUser({});
      // Every tab shares the session cookie. If another tab has signed in as someone else since
      // this one loaded its user, this tab's requests now run as them while the page still shows
      // the user it loaded (with their filters and permissions), so reload it.
      const loadedUser = Loadable.getOrElse(undefined, userStore.currentUser.get());
      if (loadedUser && loadedUser.id !== user.id) reloadPage();
      authStore.setAuth({ isAuthenticated: true });
      authStore.setAuthChecked();
      return true;
    } catch (e) {
      if (isRemoteUserTokenExpired(e)) {
        authStore.setAuth({ isAuthenticated: false });
        redirectToSSO();
        return false;
      }
      if (!isAuthFailure(e)) {
        // The master could not say; keep what we knew.
        handleError(e, { silent: true });
        authStore.setAuthChecked();
        return authStore.isAuthenticated.get();
      }
    }

    // The browser is not signed in.
    authStore.setAuth({ isAuthenticated: false });
    if (info.externalLoginUri) {
      redirectToExternalSignin();
      return false;
    }
    if (!hardLogout && info.ssoProviders?.some((ssoProvider) => ssoProvider.alwaysRedirect)) {
      redirectToSSO();
      return false;
    }
    authStore.setAuthChecked();
    return false;
  }, [
    info.externalLoginUri,
    info.ssoProviders,
    searchParams,
    redirectToExternalSignin,
    redirectToSSO,
  ]);

  return checkAuth;
};

export default useAuthCheck;
