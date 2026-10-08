import Alert from 'hew/Alert';
import Button from 'hew/Button';
import Input from 'hew/Input';
import { Modal } from 'hew/Modal';
import Row from 'hew/Row';
import Select, { Option } from 'hew/Select';
import { Loadable } from 'hew/utils/loadable';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import PoolAccessResults, {
  CHANGE_RUNNING_MESSAGE,
  changeStoppedMessage,
  PoolAccessRunner,
} from 'components/PoolAccessResults';
import {
  getGroup,
  getGroups,
  grantResourcePoolAccess,
  revokeResourcePoolAccess,
} from 'services/api';
import { V1GroupSearchResult } from 'services/api-ts-sdk';
import userStore from 'stores/users';
import { ResourcePoolAccess, ResourcePoolAccessMode } from 'types';
import handleError from 'utils/error';
import { useObservable } from 'utils/observable';
import {
  changeUsersInPools,
  PoolAccessCandidate,
  poolAccessErrorMessage,
  PoolAccessResult,
  PoolAccessUsersAction,
  resolveUsernames,
} from 'utils/resourcePoolAccess';
import { pluralizer } from 'utils/string';

import css from './PoolAccessUsersModal.module.scss';

/** The groups API returns at most this many groups per request. */
const GROUPS_PAGE_SIZE = 500;
/** The preview lists at most this many usernames. */
const PREVIEW_LIMIT = 200;

export const GRANT_GROUP_NOTE =
  'A group is expanded to its members once, when you apply: each member gets a grant of their ' +
  "own. Later changes to the group's membership do not change any grant.";
export const REVOKE_GROUP_NOTE =
  "A group is expanded to its members once, when you apply: each member's own grant is " +
  'revoked. Users who join the group later are not affected.';
export const MEMBERSHIP_CHANGED_MESSAGE =
  'Group membership changed since the preview. Check the updated list and apply again.';

interface Props {
  action: PoolAccessUsersAction;
  closeModal: () => void;
  pools: ResourcePoolAccess[];
  /**
   * Sends the change, unless another change runs. The change goes on and keeps its results when
   * the modal is closed.
   */
  runChange: PoolAccessRunner;
}

const fetchAllGroups = async (signal: AbortSignal): Promise<V1GroupSearchResult[]> => {
  const groups: V1GroupSearchResult[] = [];
  for (let offset = 0; ; offset += GROUPS_PAGE_SIZE) {
    const response = await getGroups({ limit: GROUPS_PAGE_SIZE, offset }, { signal });
    const page = response.groups ?? [];
    groups.push(...page);
    const total = response.pagination?.total ?? 0;
    if (page.length === 0 || groups.length >= total) return groups;
  }
};

const fetchGroupMembers = async (groupId: number): Promise<PoolAccessCandidate[]> => {
  const response = await getGroup({ groupId });
  return (response.group.users ?? []).map((user) => ({
    active: user.active,
    admin: user.admin,
    username: user.username,
  }));
};

const fetchMembersOf = async (
  groupIds: readonly number[],
): Promise<Map<number, PoolAccessCandidate[]>> => {
  const entries = await Promise.all(
    groupIds.map(async (groupId) => [groupId, await fetchGroupMembers(groupId)] as const),
  );
  return new Map(entries);
};

/**
 * Grants or revokes access to the selected pools for a combination of users, built-in user groups
 * expanded to their members, and pasted usernames. The master stores grants per user; a group is
 * only a way to pick users.
 */
const PoolAccessUsersModalComponent: React.FC<Props> = ({
  action,
  closeModal,
  pools,
  runChange,
}: Props) => {
  const isGrant = action === 'grant';
  const users = Loadable.getOrElse([], useObservable(userStore.getUsers()));
  const [groups, setGroups] = useState<V1GroupSearchResult[]>([]);
  const [groupsError, setGroupsError] = useState<string>();
  const [pickedUserIds, setPickedUserIds] = useState<number[]>([]);
  const [pickedGroupIds, setPickedGroupIds] = useState<number[]>([]);
  const [pasted, setPasted] = useState('');
  const [members, setMembers] = useState<Map<number, PoolAccessCandidate[]>>(new Map());
  const [membersError, setMembersError] = useState<string>();
  /** Bumped by "Try again" after the groups could not be expanded. */
  const [expandAttempt, setExpandAttempt] = useState(0);
  const [notice, setNotice] = useState<string>();
  /** Why the last change stopped, when it threw. */
  const [stopped, setStopped] = useState<string>();
  const [isApplying, setIsApplying] = useState(false);
  const [results, setResults] = useState<PoolAccessResult[]>();
  /** Set when the modal is closed: a change that was not sent yet is then not sent. */
  const closed = useRef(false);

  useEffect(() => {
    closed.current = false;
    return () => {
      closed.current = true;
    };
  }, []);

  useEffect(() => {
    const canceler = new AbortController();
    fetchAllGroups(canceler.signal)
      .then(setGroups)
      .catch((e) => {
        if (!canceler.signal.aborted) setGroupsError(poolAccessErrorMessage(e));
      });
    return () => canceler.abort();
  }, []);

  // The preview expands the picked groups that it has not expanded yet. A newer preview, also one
  // that needs no request, ignores the answer of an older one.
  useEffect(() => {
    const missing = pickedGroupIds.filter((groupId) => !members.has(groupId));
    setMembersError(undefined);
    if (missing.length === 0) return;
    let isCurrent = true;
    fetchMembersOf(missing)
      .then((fetched) => {
        if (!isCurrent) return;
        setMembersError(undefined);
        setMembers((prev) => new Map([...prev, ...fetched]));
      })
      .catch((e) => {
        if (isCurrent) setMembersError(poolAccessErrorMessage(e));
      });
    return () => {
      isCurrent = false;
    };
  }, [expandAttempt, members, pickedGroupIds]);

  const previewMembers = useMemo(
    () => pickedGroupIds.map((groupId) => members.get(groupId) ?? []),
    [members, pickedGroupIds],
  );
  const isExpanding = pickedGroupIds.some((groupId) => !members.has(groupId)) && !membersError;
  const resolved = useMemo(
    () => resolveUsernames(users, pickedUserIds, previewMembers, pasted),
    [pasted, pickedUserIds, previewMembers, users],
  );

  const poolNames = useMemo(() => pools.map((pool) => pool.poolName), [pools]);
  const publicPools = pools
    .filter((pool) => pool.mode === ResourcePoolAccessMode.Public)
    .map((pool) => pool.poolName);

  const handleApply = useCallback(async () => {
    setIsApplying(true);
    setNotice(undefined);
    setStopped(undefined);
    try {
      // Expand the groups again: the grant is made for their members at this moment.
      let fresh: Map<number, PoolAccessCandidate[]>;
      try {
        fresh = await fetchMembersOf(pickedGroupIds);
      } catch (e) {
        setMembersError(poolAccessErrorMessage(e));
        return;
      }
      // Closing the modal before anything was sent cancels the change.
      if (closed.current) return;
      const applied = resolveUsernames(
        users,
        pickedUserIds,
        pickedGroupIds.map((groupId) => fresh.get(groupId) ?? []),
        pasted,
      );
      if (applied.usernames.join('\n') !== resolved.usernames.join('\n')) {
        setMembers((prev) => new Map([...prev, ...fresh]));
        setNotice(MEMBERSHIP_CHANGED_MESSAGE);
        return;
      }
      if (applied.unknown.length > 0 || applied.usernames.length === 0) return;
      const answer = await runChange(action, (signal) =>
        changeUsersInPools(
          poolNames,
          applied.usernames,
          isGrant ? grantResourcePoolAccess : revokeResourcePoolAccess,
          { signal },
        ),
      );
      if (answer) setResults(answer);
      else setNotice(CHANGE_RUNNING_MESSAGE);
    } catch (e) {
      setStopped(poolAccessErrorMessage(e));
      handleError(e, { publicSubject: 'Pool access change stopped.', silent: true });
    } finally {
      setIsApplying(false);
    }
  }, [
    action,
    isGrant,
    pasted,
    pickedGroupIds,
    pickedUserIds,
    poolNames,
    resolved,
    runChange,
    users,
  ]);

  const verb = isGrant ? 'Grant' : 'Revoke';
  const title = `${verb} access to ${pools.length} ${pluralizer(pools.length, 'pool')}`;
  const count = resolved.usernames.length;
  const canApply =
    count > 0 && resolved.unknown.length === 0 && !isExpanding && !membersError && !isApplying;

  const footer = results ? (
    <Row>
      <Button type="primary" onClick={closeModal}>
        Close
      </Button>
    </Row>
  ) : (
    <Row>
      <Button disabled={isApplying} onClick={closeModal}>
        Cancel
      </Button>
      <Button
        danger={!isGrant}
        disabled={!canApply}
        loading={isApplying}
        type="primary"
        onClick={handleApply}>
        {isGrant
          ? `Grant to ${count} ${pluralizer(count, 'user')}`
          : `Revoke from ${count} ${pluralizer(count, 'user')}`}
      </Button>
    </Row>
  );

  if (results) {
    return (
      <Modal footer={footer} size="medium" title={title} onClose={closeModal}>
        <PoolAccessResults action={action} results={results} />
      </Modal>
    );
  }

  const shown = resolved.usernames.slice(0, PREVIEW_LIMIT);
  const flags = (username: string): string => {
    const marks = [
      resolved.inactive.includes(username) ? 'inactive' : '',
      resolved.admins.includes(username) ? 'admin' : '',
    ].filter((mark) => mark);
    return marks.length > 0 ? ` (${marks.join(', ')})` : '';
  };

  return (
    <Modal footer={footer} size="medium" title={title} onClose={closeModal}>
      <div className={css.base}>
        <p className={css.pools} data-testid="pool-access-modal-pools">
          {isGrant ? 'Grant access to ' : 'Revoke access to '}
          {poolNames.join(', ')}
        </p>
        {isGrant ? (
          <p className={css.note}>
            Grants are stored per user. A restricted pool can be used by administrators and the
            users granted access.
            {publicPools.length > 0 &&
              ` ${publicPools.join(', ')} ${publicPools.length === 1 ? 'is' : 'are'} public: ` +
                'the grants have no effect until the pool is restricted.'}
          </p>
        ) : (
          <p className={css.note}>
            Revoking applies from the next request: work that already runs or is queued in the pool
            is not stopped. Revoking a user without a grant changes nothing.
          </p>
        )}
        <div className={css.field}>
          <label htmlFor="pool-access-users">Users</label>
          <Select
            disabled={isApplying}
            id="pool-access-users"
            mode="multiple"
            placeholder="Find and select users"
            value={pickedUserIds}
            onChange={(value) => {
              setNotice(undefined);
              setPickedUserIds(value as number[]);
            }}>
            {users.map((user) => {
              const label =
                (user.displayName ? `${user.displayName} (${user.username})` : user.username) +
                (user.isActive ? '' : ' (inactive)');
              return (
                <Option key={user.id} label={label} value={user.id}>
                  {label}
                </Option>
              );
            })}
          </Select>
        </div>
        <div className={css.field}>
          <label htmlFor="pool-access-groups">Groups</label>
          <Select
            disabled={isApplying}
            id="pool-access-groups"
            mode="multiple"
            placeholder="Select user groups"
            value={pickedGroupIds}
            onChange={(value) => {
              setNotice(undefined);
              setPickedGroupIds(value as number[]);
            }}>
            {groups.map((group) => {
              const label = `${group.group.name} (${group.numMembers} ${pluralizer(
                group.numMembers,
                'member',
              )})`;
              return (
                <Option key={group.group.groupId} label={label} value={group.group.groupId}>
                  {label}
                </Option>
              );
            })}
          </Select>
          {groupsError && <Alert message={`Unable to list groups: ${groupsError}`} type="error" />}
          <p className={css.hint}>{isGrant ? GRANT_GROUP_NOTE : REVOKE_GROUP_NOTE}</p>
        </div>
        {/* hew's TextArea takes no id, so the label wraps it instead of naming it. */}
        <label className={css.field}>
          <span>Paste usernames</span>
          <Input.TextArea
            disabled={isApplying}
            placeholder="One username per line, or separated by commas or spaces"
            rows={3}
            value={pasted}
            onChange={(e) => {
              setNotice(undefined);
              setPasted(e.target.value);
            }}
          />
        </label>
        {membersError && (
          <Alert
            action={
              <Button size="small" onClick={() => setExpandAttempt((attempt) => attempt + 1)}>
                Try again
              </Button>
            }
            message={`Unable to expand the groups: ${membersError}`}
            type="error"
          />
        )}
        {notice && <Alert message={notice} type="warning" />}
        {stopped && <Alert message={changeStoppedMessage(stopped)} type="error" />}
        {resolved.unknown.length > 0 && (
          <Alert
            description={resolved.unknown.join(', ')}
            message={`${resolved.unknown.length} unknown ${pluralizer(
              resolved.unknown.length,
              'username',
            )}: remove ${resolved.unknown.length === 1 ? 'it' : 'them'} to continue. The master refuses a request that names an unknown user.`}
            type="error"
          />
        )}
        <div className={css.preview} data-testid="pool-access-preview">
          <p>
            {isExpanding
              ? 'Expanding groups...'
              : `${count} ${pluralizer(count, 'user')} after removing duplicates ` +
                `(${resolved.counts.fromUsers} picked, ${resolved.counts.fromGroups} from ` +
                `groups, ${resolved.counts.fromPaste} pasted)` +
                (resolved.inactive.length > 0 ? `, ${resolved.inactive.length} inactive` : '') +
                (resolved.admins.length > 0
                  ? `, ${resolved.admins.length} ${pluralizer(
                      resolved.admins.length,
                      'administrator',
                    )}, who may use every pool anyway`
                  : '') +
                '.'}
          </p>
          {shown.length > 0 && (
            <ul className={css.usernames}>
              {shown.map((username) => (
                <li key={username}>
                  {username}
                  {flags(username)}
                </li>
              ))}
              {resolved.usernames.length > shown.length && (
                <li>and {resolved.usernames.length - shown.length} more</li>
              )}
            </ul>
          )}
        </div>
      </div>
    </Modal>
  );
};

export default PoolAccessUsersModalComponent;
