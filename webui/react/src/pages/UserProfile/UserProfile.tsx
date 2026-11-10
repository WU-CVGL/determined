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
import { V1ResourceAllocationAggregationPeriod } from 'services/api-ts-sdk';
import userStore from 'stores/users';
import { UserResources, UserUsage } from 'types';
import handleError from 'utils/error';
import { useObservable } from 'utils/observable';
import { isAborted } from 'utils/service';

import {
  DAY_FORMAT,
  MONTH_FORMAT,
  PERIOD_COLUMN,
  ResourceMode,
  todayUtc,
  USAGE_DAYS,
  USAGE_MONTHS,
  usageProjection,
  UsageProjection,
  usageRange,
  UsageRow,
} from './usage';
import UsageCalendar from './UsageCalendar';
import css from './UserProfile.module.scss';

export const RESOURCES_POLL_INTERVAL = 15_000;

interface ResourcesProps {
  projection?: UsageProjection;
  resources: Loadable<UserResources>;
}

const CurrentResources: React.FC<ResourcesProps> = ({ projection, resources }) => (
  <Section loading={Loadable.isNotLoaded(resources)} title="Current resources">
    {Loadable.match(resources, {
      Failed: () => null,
      Loaded: (resources) =>
        projection && (
          <Card.Group size="small">
            {projection.cards(resources).map((card) => (
              <OverviewStats key={card.key} title={card.title}>
                {card.value}
              </OverviewStats>
            ))}
          </Card.Group>
        ),
      NotLoaded: () => null,
    })}
  </Section>
);

interface UsageProps {
  groupBy: GroupBy;
  projection: UsageProjection;
  usage: UserUsage;
}

const UsageView: React.FC<UsageProps> = ({ groupBy, projection, usage }) => {
  const view = useMemo(
    () => projection.usage(groupBy, usage.entries),
    [groupBy, projection, usage.entries],
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

  return (
    <>
      <ClusterHistoricalUsageChart
        chartKey={groupBy === GroupBy.Month ? 1 : 0}
        dateRange={dateRange}
        groupBy={groupBy}
        hoursByLabel={view.hoursByLabel}
        label="Hours"
        time={view.time}
      />
      <ResponsiveTable<UsageRow>
        columns={view.columns.map((column) => ({
          align: column.numeric ? ('right' as const) : undefined,
          dataIndex: column.key,
          key: column.key,
          title: column.title,
        }))}
        dataSource={view.rows}
        pagination={false}
        rowKey={PERIOD_COLUMN}
        size="small"
      />
    </>
  );
};

interface UsageSectionsProps {
  projection?: UsageProjection;
  userId: number;
}

/** The calendar and the usage; empty until the projection is known. */
const UserUsageSections: React.FC<UsageSectionsProps> = ({ projection, userId }) => {
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

  const shown = useMemo(
    () =>
      projection
        ? Loadable.map(usage, (usage) => ({ projection, usage }))
        : Loadable.flatMap(usage, () => NotLoaded),
    [projection, usage],
  );
  const calendar = useMemo(
    () => Loadable.map(shown, ({ projection, usage }) => projection.calendar(usage.entries)),
    [shown],
  );

  return (
    <>
      <Section
        id="active-days"
        loading={Loadable.isNotLoaded(calendar)}
        title={Loadable.match(calendar, {
          _: () => 'Active days',
          Loaded: ({ activeDays }) => `Active days · ${activeDays}`,
        })}>
        {Loadable.match(calendar, {
          _: () => null,
          Loaded: ({ weeks }) =>
            projection && <UsageCalendar dayValues={projection.day} weeks={weeks} />,
        })}
      </Section>
      <Section
        loading={Loadable.isNotLoaded(shown)}
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
        {Loadable.match(shown, {
          _: () => null,
          Loaded: ({ projection, usage }) => (
            <UsageView groupBy={groupBy} projection={projection} usage={usage} />
          ),
        })}
      </Section>
    </>
  );
};

/** The current resources, the calendar and the usage of a user, in one resource mode. */
const UserResourceSections: React.FC<{ userId: number }> = ({ userId }) => {
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

  // Aux is shown where the cluster meters it; without the resources, GPUs only.
  const auxMetered = Loadable.match(resources, {
    Failed: () => false,
    Loaded: (resources) => resources.auxMetered,
    NotLoaded: () => undefined,
  });
  const projection = useMemo(
    () => (auxMetered === undefined ? undefined : usageProjection(ResourceMode.Both, auxMetered)),
    [auxMetered],
  );

  return (
    <>
      <CurrentResources projection={projection} resources={resources} />
      <UserUsageSections projection={projection} userId={userId} />
    </>
  );
};

const UserProfile: React.FC = () => {
  const { userId: userIdParam = '' } = useParams<{ userId: string }>();
  const userId = /^\d+$/.test(userIdParam) ? Number(userIdParam) : undefined;
  const loadableUser = useObservable(useMemo(() => userStore.getUser(userId ?? -1), [userId]));
  const loadableUsers = useObservable(useMemo(() => userStore.getUsers(), []));

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
          <UserResourceSections key={userId} userId={userId} />
        </div>
      )}
    </Page>
  );
};

export default UserProfile;
