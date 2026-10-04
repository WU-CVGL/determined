import { useObservable } from 'micro-observables';
import { useCallback, useInsertionEffect, useRef } from 'react';
import { useSearchParams } from 'react-router-dom';

import { samlUrl } from 'ee/SamlAuth';
import { paths, routeAll } from 'routes/utils';
import { getCurrentUser, storeSessionToken } from 'services/api';
import authStore from 'stores/auth';
import determinedStore from 'stores/determinedInfo';
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
 * hands over a token in the URL instead (`?jwt=`); the master turns it into the cookie.
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

    // check if the user clicked the logout button, which ignores SSO redirection
    const hardLogout = window.location.href.includes('hard_logout=true');

    try {
      await getCurrentUser({});
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
