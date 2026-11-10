import Message from 'hew/Message';
import Spinner from 'hew/Spinner';
import { Loadable } from 'hew/utils/loadable';
import React, { useEffect, useState } from 'react';
import { Navigate, useParams } from 'react-router-dom';

import Badge from 'components/Badge';
import Link from 'components/Link';
import Page from 'components/Page';
import UserBadge from 'components/UserBadge';
import { paths } from 'routes/utils';
import { getUser } from 'services/api';
import userStore from 'stores/users';
import { DetailedUser } from 'types';
import handleError from 'utils/error';
import { useObservable } from 'utils/observable';
import { isAborted, isNotFound } from 'utils/service';

import UserOverview from './UserOverview';
import css from './UserProfile.module.scss';

type UserState = { userId?: number } & (
  | { state: 'loading' }
  | { state: 'loaded'; user: DetailedUser }
  | { state: 'notFound' }
  | { state: 'failed' }
);

const parseUserId = (param: string): number | undefined =>
  /^\d+$/.test(param) ? Number(param) : undefined;

/** The page of another user; the signed-in user's own page is Home. */
const UserProfile: React.FC = () => {
  const { userId: userIdParam = '' } = useParams<{ userId: string }>();
  const userId = parseUserId(userIdParam);
  const currentUserId = Loadable.getOrElse(undefined, useObservable(userStore.currentUser))?.id;
  const isOwn = currentUserId !== undefined && currentUserId === userId;
  const [user, setUser] = useState<UserState>({ state: 'loading', userId });

  useEffect(() => {
    if (userId === undefined || currentUserId === undefined || isOwn) return;
    const canceler = new AbortController();
    getUser({ userId }, { signal: canceler.signal })
      .then((user) => {
        if (!canceler.signal.aborted) setUser({ state: 'loaded', user, userId });
      })
      .catch((e) => {
        if (canceler.signal.aborted || isAborted(e)) return;
        if (isNotFound(e)) {
          setUser({ state: 'notFound', userId });
          return;
        }
        handleError(e, { publicSubject: 'Unable to fetch user.', silent: true });
        setUser({ state: 'failed', userId });
      });
    return () => canceler.abort();
  }, [currentUserId, isOwn, userId]);

  if (isOwn) return <Navigate replace to={paths.dashboard()} />;

  // Until the answer for this id, the page is loading.
  const shown: UserState =
    userId === undefined
      ? { state: 'notFound', userId }
      : user.userId === userId
        ? user
        : { state: 'loading', userId };
  const username = shown.state === 'loaded' ? shown.user.username : undefined;

  return (
    <Page breadcrumb={[]} docTitle={username} id="userProfile" title="User">
      {shown.state === 'notFound' ? (
        <Message title="User not found" />
      ) : shown.state === 'failed' ? (
        <Message icon="warning" title="Unable to fetch user" />
      ) : shown.state === 'loading' || userId === undefined ? (
        <Spinner center spinning />
      ) : (
        <div className={css.base}>
          <div className={css.header}>
            <div className={css.user}>
              <UserBadge user={shown.user} />
              {!shown.user.isActive && <Badge>Deactivated</Badge>}
            </div>
            <Link path={`${paths.jobs()}?user=${userId}`}>Jobs</Link>
          </div>
          <UserOverview userId={userId} />
        </div>
      )}
    </Page>
  );
};

export default UserProfile;
