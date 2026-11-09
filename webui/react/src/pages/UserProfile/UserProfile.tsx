import dayjs from 'dayjs';
import Card from 'hew/Card';
import Message from 'hew/Message';
import RadioGroup from 'hew/RadioGroup';
import Spinner from 'hew/Spinner';
import { Failed, Loadable, Loaded, NotLoaded } from 'hew/utils/loadable';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useParams } from 'react-router-dom';

import Badge from 'components/Badge';
import Link from 'components/Link';
import OverviewStats from 'components/OverviewStats';
import Page from 'components/Page';
import Section from 'components/Section';
import ResponsiveTable from 'components/Table/ResponsiveTable';
import UserBadge from 'components/UserBadge';
import usePolling from 'hooks/usePolling';
import { GroupBy } from 'pages/Cluster/ClusterHistoricalUsage.settings';
import ClusterHistoricalUsageChart from 'pages/Cluster/ClusterHistoricalUsageChart';
import { paths } from 'routes/utils';
import { getUserResources, getUserUsage } from 'services/api';
import { V1ResourceAllocationAggregationPeriod, V1ResourcePoolType } from 'services/api-ts-sdk';
import clusterStore from 'stores/cluster';
import userStore from 'stores/users';
import { ResourcePool, ResourceType, UserResources, UserUsage, UserUsageEntry } from 'types';
import handleError from 'utils/error';
import { useObservable } from 'utils/observable';
import { isAborted } from 'utils/service';

import {
  DAY_FORMAT,
  formatHours,
  lastDays,
  lastMonths,
  MONTH_FORMAT,
  todayUtc,
  toHours,
  totalResources,
  USAGE_DAYS,
  USAGE_MONTHS,
  usageRange,
} from './usage';
import UsageCalendar from './UsageCalendar';
import css from './UserProfile.module.scss';

export const RESOURCES_POLL_INTERVAL = 15_000;

const anyCpuPool = (pools: ResourcePool[]): boolean =>
  pools.some((pool) => pool.slotType === ResourceType.CPU);

interface BlockProps {
  pools: ResourcePool[];
  userId: number;
}

const CurrentResources: React.FC<BlockProps> = ({ pools, userId }: BlockProps) => {
  const [resources, setResources] = useState<Loadable<UserResources>>(NotLoaded);
  const hasLoaded = useRef(false);

  const fetchResources = useCallback(async () => {
    try {
      const response = await getUserResources({ userId });
      hasLoaded.current = true;
      setResources(Loaded(response));
    } catch (e) {
      // A failed refresh keeps the last counts.
      handleError(e, {
        publicSubject: 'Unable to fetch current resources.',
        silent: hasLoaded.current,
      });
      if (!hasLoaded.current) setResources(Failed(e as Error));
    }
  }, [userId]);

  usePolling(fetchResources, { interval: RESOURCES_POLL_INTERVAL });

  const allKubernetes =
    pools.length > 0 && pools.every((pool) => pool.type === V1ResourcePoolType.K8S);

  return (
    <Section loading={Loadable.isNotLoaded(resources)} title="Current resources">
      {Loadable.match(resources, {
        Failed: () => null,
        Loaded: ({ pools: held }) => {
          const total = totalResources(held);
          return (
            <Card.Group size="small">
              <OverviewStats title="GPUs">{total.gpus}</OverviewStats>
              {(total.cpuSlots > 0 || anyCpuPool(pools)) && (
                <OverviewStats title="CPU slots">{total.cpuSlots}</OverviewStats>
              )}
              <OverviewStats title="Aux containers">
                {allKubernetes ? 'Not available' : total.auxContainers ?? 0}
              </OverviewStats>
            </Card.Group>
          );
        },
        NotLoaded: () => null,
      })}
    </Section>
  );
};

const usageColumns = (groupBy: GroupBy, showCpuSlots: boolean, showAux: boolean) =>
  [
    {
      dataIndex: 'periodStart',
      key: 'periodStart',
      title: groupBy === GroupBy.Month ? 'Month' : 'Date',
    },
    {
      align: 'right' as const,
      dataIndex: 'gpuSeconds',
      key: 'gpuSeconds',
      render: (seconds: number) => formatHours(seconds),
      title: 'GPU hours',
    },
    showCpuSlots && {
      align: 'right' as const,
      dataIndex: 'cpuSlotSeconds',
      key: 'cpuSlotSeconds',
      render: (seconds: number) => formatHours(seconds),
      title: 'CPU slot hours',
    },
    showAux && {
      align: 'right' as const,
      dataIndex: 'auxContainerSeconds',
      key: 'auxContainerSeconds',
      render: (seconds?: number) => formatHours(seconds ?? 0),
      title: 'Aux container hours',
    },
  ].filter((column): column is Exclude<typeof column, false> => !!column);

interface UsageProps {
  showCpuSlots: boolean;
  usage: UserUsage;
}

const UsageView: React.FC<UsageProps & { groupBy: GroupBy }> = ({
  groupBy,
  showCpuSlots,
  usage,
}) => {
  const showAux = usage.entries.some((entry) => entry.auxContainerSeconds !== undefined);
  const entries = useMemo(
    () => (groupBy === GroupBy.Month ? lastMonths(usage.entries) : lastDays(usage.entries)),
    [groupBy, usage.entries],
  );
  const dateRange = useMemo((): [number, number] => {
    // The chart reads its time strings as local dates, and so does its range.
    const today = todayUtc();
    const [first, format] =
      groupBy === GroupBy.Month
        ? [today.subtract(USAGE_MONTHS - 1, 'month'), MONTH_FORMAT]
        : [today.subtract(USAGE_DAYS - 1, 'day'), DAY_FORMAT];
    return [dayjs(first.format(format)).unix(), dayjs(today.format(format)).unix()];
  }, [groupBy]);

  const hoursByLabel: Record<string, number[]> = {
    GPU: entries.map((entry) => toHours(entry.gpuSeconds)),
  };
  if (showCpuSlots) {
    hoursByLabel['CPU slots'] = entries.map((entry) => toHours(entry.cpuSlotSeconds));
  }
  if (showAux) {
    hoursByLabel['Aux'] = entries.map((entry) => toHours(entry.auxContainerSeconds ?? 0));
  }

  return (
    <>
      <ClusterHistoricalUsageChart
        chartKey={groupBy === GroupBy.Month ? 1 : 0}
        dateRange={dateRange}
        groupBy={groupBy}
        hoursByLabel={hoursByLabel}
        label="Hours"
        time={entries.map((entry) => entry.periodStart)}
      />
      <ResponsiveTable<UserUsageEntry>
        columns={usageColumns(groupBy, showCpuSlots, showAux)}
        dataSource={[...entries].reverse()}
        pagination={false}
        rowKey="periodStart"
        size="small"
      />
    </>
  );
};

const UserUsageSections: React.FC<BlockProps> = ({ pools, userId }: BlockProps) => {
  const [usage, setUsage] = useState<Loadable<UserUsage>>(NotLoaded);
  const [groupBy, setGroupBy] = useState<GroupBy>(GroupBy.Day);

  // One request per user: the calendar, the days and the months all come from it.
  useEffect(() => {
    const canceler = new AbortController();
    const { endDate, startDate } = usageRange();
    getUserUsage(
      {
        endDate,
        period: V1ResourceAllocationAggregationPeriod.DAILY,
        startDate,
        userId,
      },
      { signal: canceler.signal },
    )
      .then((response) => setUsage(Loaded(response)))
      .catch((e) => {
        if (canceler.signal.aborted || isAborted(e)) return;
        handleError(e, { publicSubject: 'Unable to fetch usage.' });
        setUsage(Failed(e as Error));
      });
    return () => canceler.abort();
  }, [userId]);

  const showCpuSlots =
    anyCpuPool(pools) ||
    Loadable.getOrElse<UserUsageEntry[]>(
      [],
      Loadable.map(usage, (u) => u.entries),
    ).some((entry) => entry.cpuSlotSeconds > 0);

  return (
    <>
      <Section loading={Loadable.isNotLoaded(usage)} title="Active days (UTC)">
        {Loadable.match(usage, {
          Failed: () => null,
          Loaded: (usage) => <UsageCalendar showCpuSlots={showCpuSlots} usage={usage} />,
          NotLoaded: () => null,
        })}
      </Section>
      <Section
        loading={Loadable.isNotLoaded(usage)}
        options={
          <RadioGroup<GroupBy>
            options={[
              { id: GroupBy.Day, label: 'Day' },
              { id: GroupBy.Month, label: 'Month' },
            ]}
            value={groupBy}
            onChange={setGroupBy}
          />
        }
        title="Usage">
        {Loadable.match(usage, {
          Failed: () => null,
          Loaded: (usage) => (
            <UsageView groupBy={groupBy} showCpuSlots={showCpuSlots} usage={usage} />
          ),
          NotLoaded: () => null,
        })}
      </Section>
    </>
  );
};

const UserProfile: React.FC = () => {
  const { userId: userIdParam = '' } = useParams<{ userId: string }>();
  const userId = /^\d+$/.test(userIdParam) ? Number(userIdParam) : undefined;
  const loadableUser = useObservable(useMemo(() => userStore.getUser(userId ?? -1), [userId]));
  const loadableUsers = useObservable(useMemo(() => userStore.getUsers(), []));
  const pools = Loadable.getOrElse([], useObservable(clusterStore.resourcePools));

  const user = Loadable.getOrElse(undefined, loadableUser);
  const notFound = userId === undefined || (!user && Loadable.isLoaded(loadableUsers));

  return (
    <Page breadcrumb={[]} docTitle={user?.username} id="userProfile" title="User">
      {notFound ? (
        <Message title="User not found" />
      ) : user === undefined || userId === undefined ? (
        <Spinner center spinning />
      ) : (
        <div className={css.base}>
          <div className={css.header}>
            <div className={css.user}>
              <UserBadge user={user} />
              {!user.isActive && <Badge>Deactivated</Badge>}
            </div>
            <Link path={`${paths.jobs()}?user=${userId}`}>Jobs</Link>
          </div>
          <CurrentResources key={`resources-${userId}`} pools={pools} userId={userId} />
          <UserUsageSections key={`usage-${userId}`} pools={pools} userId={userId} />
        </div>
      )}
    </Page>
  );
};

export default UserProfile;
