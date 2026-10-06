import { Table } from 'antd';
import Alert from 'hew/Alert';
import Badge from 'hew/Badge';
import Button from 'hew/Button';
import Icon from 'hew/Icon';
import Input from 'hew/Input';
import { useModal } from 'hew/Modal';
import Row from 'hew/Row';
import Tooltip from 'hew/Tooltip';
import { Loadable, Loaded, NotLoaded } from 'hew/utils/loadable';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import PoolAccessConfirmModalComponent from 'components/PoolAccessConfirmModal';
import PoolAccessResults, {
  PoolAccessResultAction,
  poolAccessResultText,
  PoolAccessRunner,
} from 'components/PoolAccessResults';
import PoolAccessUsersModalComponent from 'components/PoolAccessUsersModal';
import Section from 'components/Section';
import InteractiveTable, { onRightClickableCell } from 'components/Table/InteractiveTable';
import SkeletonTable from 'components/Table/SkeletonTable';
import { defaultRowClassName, getFullPaginationConfig } from 'components/Table/Table';
import { useSettings } from 'hooks/useSettings';
import {
  getResourcePoolAccess,
  revokeResourcePoolAccess,
  setResourcePoolAccessMode,
} from 'services/api';
import userStore from 'stores/users';
import { ResourcePoolAccess, ResourcePoolAccessMode, ResourcePoolAccessUser } from 'types';
import handleError from 'utils/error';
import {
  changeUsersInPools,
  matchesPoolSearch,
  poolAccessErrorMessage,
  PoolAccessResult,
  PoolAccessUsersAction,
  resourcePoolAccessWarnings,
  setModeInPools,
} from 'utils/resourcePoolAccess';
import { alphaNumericSorter, numericSorter } from 'utils/sort';
import { pluralizer } from 'utils/string';

import css from './PoolAccess.module.scss';
import settingsConfig, { DEFAULT_COLUMN_WIDTHS, DEFAULT_COLUMNS } from './PoolAccess.settings';

const RESTRICTED_COLOR = { h: 32, l: 50, s: 90 };

export const RESTRICT_INTRO =
  'Only administrators and the users granted access can start new work in a restricted pool.';
export const RESTRICT_RUNNING_NOTE =
  'Work that already runs or is queued in these pools is not stopped, paused, or moved. Users ' +
  'without a grant can no longer submit, activate, unpause, continue, fork, or clone work into ' +
  'them, move a job into them, or make them a workspace default.';
export const PUBLIC_INTRO = 'Every user can start work in a public pool.';
export const PENDING_DISMISSED_NOTE =
  'A change whose dialog was closed is still being applied. Its results will show here.';
export const REVOKE_RUNNING_NOTE =
  'Revoking applies from the next request: work that already runs or is queued in the pool is ' +
  'not stopped.';

const ACTION_TITLES: Record<PoolAccessResultAction, string> = {
  grant: 'Grant',
  public: 'Make public',
  restrict: 'Restrict',
  revoke: 'Revoke',
};

const modeLabel = (mode: ResourcePoolAccess['mode']): string =>
  mode === ResourcePoolAccessMode.Restricted ? 'Restricted' : 'Public';

const ModeBadge: React.FC<{ mode: ResourcePoolAccess['mode'] }> = ({ mode }) =>
  mode === ResourcePoolAccessMode.Restricted ? (
    <Badge backgroundColor={RESTRICTED_COLOR} text={modeLabel(mode)} />
  ) : (
    <Badge text={modeLabel(mode)} />
  );

const inactiveCount = (pool: ResourcePoolAccess): number =>
  pool.users.filter((user) => !user.active).length;

/** What restricting the pool means for its users, for the confirmation. */
const restrictedAccessText = (pool: ResourcePoolAccess): string => {
  if (pool.users.length === 0) return 'no grants: only administrators can use it';
  const inactive = inactiveCount(pool);
  return (
    `${pool.users.length} granted ${pluralizer(pool.users.length, 'user')} can use it` +
    (inactive > 0 ? ` (${inactive} inactive)` : '')
  );
};

interface DetailProps {
  onRevoke: (pool: ResourcePoolAccess, usernames: string[]) => void;
  pool: ResourcePoolAccess;
}

/** The expanded row of a pool: its granted users, its workspace defaults, and its warnings. */
const PoolAccessDetail: React.FC<DetailProps> = ({ onRevoke, pool }: DetailProps) => {
  const [selected, setSelected] = useState<string[]>([]);
  const warnings = resourcePoolAccessWarnings(pool);
  const granted = useMemo(
    () => [...pool.users].sort((a, b) => alphaNumericSorter(a.username, b.username)),
    [pool.users],
  );

  useEffect(() => {
    // Keep only the selected users that are still granted.
    const names = new Set(pool.users.map((user) => user.username));
    setSelected((prev) => prev.filter((name) => names.has(name)));
  }, [pool.users]);

  return (
    <div className={css.detail} data-testid={`pool-access-detail-${pool.poolName}`}>
      <div className={css.detailSection}>
        <Row justifyContent="space-between">
          <strong>
            Granted users ({pool.users.length})
            {pool.mode === ResourcePoolAccessMode.Public &&
              pool.users.length > 0 &&
              ': no effect while the pool is public'}
          </strong>
          <Button danger disabled={selected.length === 0} onClick={() => onRevoke(pool, selected)}>
            {`Revoke selected (${selected.length})`}
          </Button>
        </Row>
        <Table<ResourcePoolAccessUser>
          columns={[
            {
              dataIndex: 'username',
              key: 'username',
              render: (_: string, user: ResourcePoolAccessUser) => (
                <span>
                  {user.username}
                  {!user.active && <span className={css.flag}> inactive</span>}
                  {user.admin && <span className={css.flag}> admin</span>}
                </span>
              ),
              title: 'User',
            },
          ]}
          dataSource={granted}
          locale={{ emptyText: 'No users are granted access.' }}
          pagination={granted.length > 20 ? { pageSize: 20 } : false}
          rowKey="username"
          rowSelection={{
            onChange: (keys) => setSelected(keys as string[]),
            selectedRowKeys: selected,
          }}
          size="small"
        />
      </div>
      <div className={css.detailSection}>
        <strong>Workspace defaults</strong>
        {pool.workspaceDefaults.length === 0 ? (
          <span className={css.muted}>No workspace uses this pool as a default.</span>
        ) : (
          <ul>
            {pool.workspaceDefaults.map((workspaceDefault) => (
              <li key={`${workspaceDefault.workspaceId}-${workspaceDefault.kind}`}>
                {workspaceDefault.workspace}: default {workspaceDefault.kind} pool
              </li>
            ))}
          </ul>
        )}
        {pool.mode === ResourcePoolAccessMode.Restricted && (
          <span className={css.muted}>
            Restricted
            {pool.restrictedBy ? ` by ${pool.restrictedBy}` : ''}
            {pool.restrictedAt ? ` at ${new Date(pool.restrictedAt).toLocaleString()}` : ''}.
          </span>
        )}
      </div>
      {warnings.length > 0 && (
        <div className={css.detailSection}>
          <strong>Warnings</strong>
          <ul className={css.warnings}>
            {warnings.map((warning) => (
              <li key={warning}>{warning}</li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
};

/** A change sent from a dialog. */
interface ChangeRun {
  /** The dialog was closed before the master answered. */
  dismissed: boolean;
}

/** The results of a change whose dialog was closed before the master answered. */
interface DismissedResults {
  action: PoolAccessResultAction;
  id: number;
  results: PoolAccessResult[];
}

/** Tells about the failed pools of a change that finished after the tab was left. */
const notifyFailures = (action: PoolAccessResultAction, results: PoolAccessResult[]): void => {
  const failed = results.filter((result) => !result.ok);
  if (failed.length === 0) return;
  handleError(new Error(`${failed.length} ${pluralizer(failed.length, 'pool')} failed`), {
    publicMessage: failed
      .map((result) => `${result.poolName}: ${poolAccessResultText(action, result)}`)
      .join('; '),
    publicSubject: `Pool access: ${ACTION_TITLES[action]} failed for ${failed.length} of ${
      results.length
    } ${pluralizer(results.length, 'pool')}`,
    silent: false,
  });
};

interface ConfirmConfig {
  action: PoolAccessResultAction;
  content: React.ReactNode;
  danger?: boolean;
  okText: string;
  run: () => Promise<PoolAccessResult[]>;
  title: string;
}

/**
 * The Admin "Pool Access" tab: who may use each resource pool, with bulk changes for the selected
 * pools. Every change goes through the master's resource pool access API.
 */
const PoolAccess: React.FC = () => {
  const [pools, setPools] = useState<Loadable<ResourcePoolAccess[]>>(NotLoaded);
  const [loadError, setLoadError] = useState<string>();
  const [search, setSearch] = useState('');
  const [selectedNames, setSelectedNames] = useState<string[]>([]);
  // The modals are mounted only while open, so they load their data when they open.
  const [usersAction, setUsersAction] = useState<PoolAccessUsersAction>();
  const [confirm, setConfirm] = useState<ConfirmConfig>();
  // The changes in flight. A change goes on when its dialog is closed or the tab is left.
  const runs = useRef(new Set<ChangeRun>());
  const [dismissed, setDismissed] = useState<DismissedResults[]>([]);
  const dismissedId = useRef(0);
  /** The changes whose dialog was closed and whose results have not come back yet. */
  const [pendingDismissed, setPendingDismissed] = useState(0);
  const isMounted = useRef(true);
  const pageRef = useRef<HTMLElement>(null);
  const canceler = useRef(new AbortController());
  const { settings, updateSettings } = useSettings(settingsConfig);

  const UsersModal = useModal(PoolAccessUsersModalComponent);
  const ConfirmModal = useModal(PoolAccessConfirmModalComponent);

  const fetchPools = useCallback(async () => {
    try {
      const response = await getResourcePoolAccess({}, { signal: canceler.current.signal });
      setPools(Loaded(response));
      setLoadError(undefined);
    } catch (e) {
      if (canceler.current.signal.aborted) return;
      setLoadError(`Unable to load resource pool access: ${poolAccessErrorMessage(e)}`);
      handleError(e, { publicSubject: 'Unable to load resource pool access.', silent: true });
    }
  }, []);

  useEffect(() => {
    fetchPools();
    // The user picker of the grant and revoke modal reads the user list.
    userStore.fetchUsers();
  }, [fetchPools]);

  useEffect(() => {
    isMounted.current = true;
    const current = canceler.current;
    return () => {
      isMounted.current = false;
      current.abort();
    };
  }, []);

  /**
   * Sends a change for a dialog and answers with its results. When the dialog was closed before
   * the master answered, the results show on the tab; when the tab was left, a notification names
   * the failed pools.
   */
  const runChange: PoolAccessRunner = useCallback(
    async (action, run) => {
      const current: ChangeRun = { dismissed: false };
      runs.current.add(current);
      try {
        const results = await run();
        if (!isMounted.current) {
          notifyFailures(action, results);
        } else if (current.dismissed) {
          const id = ++dismissedId.current;
          setDismissed((prev) => [...prev, { action, id, results }]);
        }
        return results;
      } finally {
        runs.current.delete(current);
        if (isMounted.current) {
          if (current.dismissed) setPendingDismissed((n) => n - 1);
          fetchPools();
        }
      }
    },
    [fetchPools],
  );

  /** Marks the changes in flight as dismissed, when their dialog is closed. */
  const dismissRuns = useCallback(() => {
    let count = 0;
    runs.current.forEach((run) => {
      if (run.dismissed) return;
      run.dismissed = true;
      count += 1;
    });
    if (count > 0) setPendingDismissed((n) => n + count);
  }, []);

  const allPools = useMemo(() => Loadable.getOrElse([], pools), [pools]);
  const filteredPools = useMemo(
    () => allPools.filter((pool) => matchesPoolSearch(pool, search)),
    [allPools, search],
  );
  const selectedPools = useMemo(
    () => allPools.filter((pool) => selectedNames.includes(pool.poolName)),
    [allPools, selectedNames],
  );

  useEffect(() => {
    // A pool that is no longer listed cannot stay selected.
    const names = new Set(allPools.map((pool) => pool.poolName));
    setSelectedNames((prev) => {
      const kept = prev.filter((name) => names.has(name));
      return kept.length === prev.length ? prev : kept;
    });
  }, [allPools]);

  const handleSearch = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      setSearch(e.target.value);
      updateSettings({ tableOffset: 0 });
    },
    [updateSettings],
  );

  const openUsersModal = useCallback(
    (action: PoolAccessUsersAction) => {
      setUsersAction(action);
      UsersModal.open();
    },
    [UsersModal],
  );

  const closeUsersModal = useCallback(() => {
    dismissRuns();
    UsersModal.close('cancel');
    setUsersAction(undefined);
  }, [UsersModal, dismissRuns]);

  const closeConfirm = useCallback(() => {
    dismissRuns();
    ConfirmModal.close('cancel');
    setConfirm(undefined);
  }, [ConfirmModal, dismissRuns]);

  const openConfirm = useCallback(
    (config: ConfirmConfig) => {
      setConfirm(config);
      ConfirmModal.open();
    },
    [ConfirmModal],
  );

  const handleRestrict = useCallback(() => {
    const targets = selectedPools;
    const warnings = targets.flatMap((pool) =>
      resourcePoolAccessWarnings(pool, ResourcePoolAccessMode.Restricted),
    );
    openConfirm({
      action: 'restrict',
      content: (
        <div className={css.confirm} data-testid="pool-access-restrict-confirm">
          <p>{RESTRICT_INTRO}</p>
          <ul>
            {targets.map((pool) => (
              <li key={pool.poolName}>
                <strong>{pool.poolName}</strong>: {restrictedAccessText(pool)}
                {pool.mode === ResourcePoolAccessMode.Restricted && ' (already restricted)'}
              </li>
            ))}
          </ul>
          <p>{RESTRICT_RUNNING_NOTE}</p>
          {warnings.length > 0 && (
            <Alert
              description={
                <ul>
                  {warnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
              }
              message="Warnings once restricted"
              type="warning"
            />
          )}
        </div>
      ),
      danger: true,
      okText: `Restrict ${targets.length} ${pluralizer(targets.length, 'pool')}`,
      run: () =>
        setModeInPools(
          targets.map((pool) => pool.poolName),
          ResourcePoolAccessMode.Restricted,
          setResourcePoolAccessMode,
        ),
      title: `Restrict ${targets.length} ${pluralizer(targets.length, 'pool')}`,
    });
  }, [openConfirm, selectedPools]);

  const handleMakePublic = useCallback(() => {
    const targets = selectedPools;
    openConfirm({
      action: 'public',
      content: (
        <div className={css.confirm} data-testid="pool-access-public-confirm">
          <p>{PUBLIC_INTRO}</p>
          <ul>
            {targets.map((pool) => (
              <li key={pool.poolName}>
                <strong>{pool.poolName}</strong>:{' '}
                {pool.mode === ResourcePoolAccessMode.Public
                  ? 'already public'
                  : pool.users.length === 0
                    ? 'no grants'
                    : `${pool.users.length} ${pluralizer(pool.users.length, 'grant')} kept`}
              </li>
            ))}
          </ul>
          <p>
            Grants are kept: they have no effect while a pool is public and apply again when the
            pool is restricted again.
          </p>
        </div>
      ),
      okText: `Make ${targets.length} ${pluralizer(targets.length, 'pool')} public`,
      run: () =>
        setModeInPools(
          targets.map((pool) => pool.poolName),
          ResourcePoolAccessMode.Public,
          setResourcePoolAccessMode,
        ),
      title: `Make ${targets.length} ${pluralizer(targets.length, 'pool')} public`,
    });
  }, [openConfirm, selectedPools]);

  const handleDetailRevoke = useCallback(
    (pool: ResourcePoolAccess, usernames: string[]) => {
      const sorted = [...usernames].sort((a, b) => alphaNumericSorter(a, b));
      openConfirm({
        action: 'revoke',
        content: (
          <div className={css.confirm} data-testid="pool-access-revoke-confirm">
            <p>
              Revoke access to <strong>{pool.poolName}</strong> from {sorted.length}{' '}
              {pluralizer(sorted.length, 'user')}: {sorted.join(', ')}.
            </p>
            <p>{REVOKE_RUNNING_NOTE}</p>
          </div>
        ),
        danger: true,
        okText: `Revoke from ${sorted.length} ${pluralizer(sorted.length, 'user')}`,
        run: () => changeUsersInPools([pool.poolName], sorted, revokeResourcePoolAccess),
        title: `Revoke access to ${pool.poolName}`,
      });
    },
    [openConfirm],
  );

  const columns = useMemo(
    () => [
      {
        dataIndex: 'poolName',
        defaultWidth: DEFAULT_COLUMN_WIDTHS['poolName'],
        key: 'poolName',
        onCell: onRightClickableCell,
        render: (_: string, pool: ResourcePoolAccess) => (
          <div className={css.poolName}>
            <span>{pool.poolName}</span>
            {!pool.exists && (
              <Tooltip content="Access records for a name that no resource manager configures and no dynamic pool is saved with. They apply to a pool created with this name.">
                <span className={css.orphan}>no pool</span>
              </Tooltip>
            )}
            {pool.defaultCompute && <span className={css.tag}>cluster default compute</span>}
            {pool.defaultAux && <span className={css.tag}>cluster default aux</span>}
          </div>
        ),
        sorter: (a: ResourcePoolAccess, b: ResourcePoolAccess) =>
          alphaNumericSorter(a.poolName, b.poolName),
        title: 'Pool',
      },
      {
        dataIndex: 'mode',
        defaultWidth: DEFAULT_COLUMN_WIDTHS['mode'],
        key: 'mode',
        onCell: onRightClickableCell,
        render: (_: string, pool: ResourcePoolAccess) => <ModeBadge mode={pool.mode} />,
        sorter: (a: ResourcePoolAccess, b: ResourcePoolAccess) =>
          alphaNumericSorter(a.mode, b.mode),
        title: 'Access',
      },
      {
        dataIndex: 'users',
        defaultWidth: DEFAULT_COLUMN_WIDTHS['users'],
        key: 'users',
        onCell: onRightClickableCell,
        render: (_: string, pool: ResourcePoolAccess) => {
          const inactive = inactiveCount(pool);
          return (
            <span data-testid={`pool-access-users-${pool.poolName}`}>
              {pool.users.length}
              {inactive > 0 && <span className={css.flag}> ({inactive} inactive)</span>}
            </span>
          );
        },
        sorter: (a: ResourcePoolAccess, b: ResourcePoolAccess) =>
          numericSorter(a.users.length, b.users.length),
        title: 'Granted users',
      },
      {
        dataIndex: 'workspaceDefaults',
        defaultWidth: DEFAULT_COLUMN_WIDTHS['workspaceDefaults'],
        key: 'workspaceDefaults',
        onCell: onRightClickableCell,
        render: (_: string, pool: ResourcePoolAccess) =>
          pool.workspaceDefaults
            .map((workspaceDefault) => `${workspaceDefault.workspace} (${workspaceDefault.kind})`)
            .join(', '),
        title: 'Workspace defaults',
      },
      {
        dataIndex: 'warnings',
        defaultWidth: DEFAULT_COLUMN_WIDTHS['warnings'],
        key: 'warnings',
        onCell: onRightClickableCell,
        render: (_: string, pool: ResourcePoolAccess) => {
          const warnings = resourcePoolAccessWarnings(pool);
          if (warnings.length === 0) return null;
          return (
            <Tooltip
              content={
                <ul className={css.tooltipList}>
                  {warnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
              }>
              <span
                className={css.warningCount}
                data-testid={`pool-access-warnings-${pool.poolName}`}>
                <Icon decorative name="warning" size="small" />
                {warnings.length}
              </span>
            </Tooltip>
          );
        },
        title: 'Warnings',
      },
    ],
    [],
  );

  const noSelection = selectedPools.length === 0;

  return (
    <>
      <Section className={css.base}>
        <div className={css.actionBar} data-testid="pool-access-actions">
          <Input
            allowClear
            placeholder="Find a pool, granted user, or workspace"
            prefix={<Icon color="cancel" decorative name="search" size="tiny" />}
            value={search}
            width="100%"
            onChange={handleSearch}
          />
          <Row>
            <span className={css.selection}>
              {selectedPools.length} {pluralizer(selectedPools.length, 'pool')} selected
            </span>
            <Button disabled={noSelection} onClick={() => openUsersModal('grant')}>
              Grant…
            </Button>
            <Button disabled={noSelection} onClick={() => openUsersModal('revoke')}>
              Revoke…
            </Button>
            <Button disabled={noSelection} onClick={handleRestrict}>
              Restrict
            </Button>
            <Button disabled={noSelection} onClick={handleMakePublic}>
              Make public
            </Button>
          </Row>
        </div>
        {loadError && <Alert message={loadError} type="error" />}
        {pendingDismissed > 0 && (
          <div className={css.notice}>
            <Alert message={PENDING_DISMISSED_NOTE} type="info" />
          </div>
        )}
        {dismissed.map(({ action, id, results }) => (
          <div className={css.notice} data-testid="pool-access-dismissed-results" key={id}>
            <Alert
              action={
                <Button
                  size="small"
                  onClick={() => setDismissed((prev) => prev.filter((item) => item.id !== id))}>
                  Dismiss
                </Button>
              }
              description={<PoolAccessResults action={action} results={results} />}
              message={`${ACTION_TITLES[action]}: finished after its dialog was closed`}
              type={results.every((result) => result.ok) ? 'success' : 'error'}
            />
          </div>
        ))}
        {settings ? (
          <InteractiveTable<ResourcePoolAccess>
            columns={columns}
            containerRef={pageRef}
            dataSource={filteredPools}
            expandable={{
              expandedRowRender: (pool: ResourcePoolAccess) => (
                <PoolAccessDetail pool={pool} onRevoke={handleDetailRevoke} />
              ),
            }}
            interactiveColumns={false}
            loading={Loadable.isNotLoaded(pools) && !loadError}
            pagination={getFullPaginationConfig(
              { limit: settings.tableLimit, offset: settings.tableOffset },
              filteredPools.length,
            )}
            rowClassName={defaultRowClassName({ clickable: false })}
            rowKey="poolName"
            rowSelection={{
              onChange: (keys) => setSelectedNames(keys as string[]),
              preserveSelectedRowKeys: true,
              selectedRowKeys: selectedNames,
            }}
            settings={{ ...settings, columns: DEFAULT_COLUMNS }}
            showSorterTooltip={false}
            size="small"
            updateSettings={updateSettings}
          />
        ) : (
          <SkeletonTable columns={columns.length} />
        )}
      </Section>
      {usersAction && (
        <UsersModal.Component
          action={usersAction}
          closeModal={closeUsersModal}
          pools={selectedPools}
          runChange={runChange}
        />
      )}
      {confirm && (
        <ConfirmModal.Component
          action={confirm.action}
          closeModal={closeConfirm}
          content={confirm.content}
          danger={confirm.danger}
          okText={confirm.okText}
          run={() => runChange(confirm.action, confirm.run)}
          title={confirm.title}
        />
      )}
    </>
  );
};

export default PoolAccess;
