import { useObservable } from 'micro-observables';
import React, { useEffect, useMemo, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';

import useAuthCheck from 'hooks/useAuthCheck';
import { paths, routeAll } from 'routes/utils';
import { logout } from 'services/api';
import authStore from 'stores/auth';
import determinedStore from 'stores/determinedInfo';
import permissionStore from 'stores/permissions';
import roleStore from 'stores/roles';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';
import workspaceStore from 'stores/workspaces';
import handleError, { ErrorLevel, ErrorType } from 'utils/error';
import { isAuthFailure } from 'utils/service';

const SignOut: React.FC = () => {
  const navigate = useNavigate();
  const location = useLocation();
  const info = useObservable(determinedStore.info);
  const [isSigningOut, setIsSigningOut] = useState(false);
  const queries = useMemo(() => new URLSearchParams(location.search), [location.search]);
  const checkAuth = useAuthCheck();

  useEffect(() => {
    const signOut = async (): Promise<void> => {
      setIsSigningOut(true);
      roleStore.reset();
      permissionStore.reset();
      userStore.reset();
      workspaceStore.reset();
      userSettings.reset();
      try {
        // The master ends the session and removes the HttpOnly session cookie, which the web UI
        // cannot do itself. It removes the cookie even when the session has already ended.
        await logout({});
      } catch (e) {
        // An authentication failure means the session had already ended. Any other failure may
        // have left the session and its cookie in place, which only the master can remove.
        if (!isAuthFailure(e)) {
          handleError(e, {
            isUserTriggered: false,
            level: ErrorLevel.Warn,
            publicMessage:
              'The master did not confirm the end of your session, so this browser may still be ' +
              'signed in. Sign out again once the master is reachable.',
            publicSubject: 'Sign-out incomplete',
            silent: false,
            type: ErrorType.Server,
          });
        }
      }
      authStore.reset();

      if (info.externalLogoutUri) {
        const isAuthenticated = await checkAuth();
        if (isAuthenticated) routeAll(info.externalLogoutUri);
      } else {
        const searchParameters = new URLSearchParams();
        searchParameters.append('r', String(Math.random()));
        if (queries.has('remote_expired'))
          searchParameters.append('remote_expired', queries.get('remote_expired') ?? 'false');
        if (queries.has('hard_logout'))
          searchParameters.append('hard_logout', queries.get('hard_logout') ?? 'false');
        navigate(paths.login() + '?' + searchParameters.toString(), { state: location.state });
      }
    };

    if (!isSigningOut) signOut();
  }, [checkAuth, navigate, info.externalLogoutUri, location.state, isSigningOut, queries]);

  return null;
};

export default SignOut;
