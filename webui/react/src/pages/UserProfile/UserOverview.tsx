import dayjs from 'dayjs';
import Card from 'hew/Card';
import RadioGroup from 'hew/RadioGroup';
import { Failed, Loadable, Loaded, NotLoaded } from 'hew/utils/loadable';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import OverviewStats from 'components/OverviewStats';
import Section from 'components/Section';
import ResponsiveTable from 'components/Table/ResponsiveTable';
import { GroupBy } from 'pages/Cluster/ClusterHistoricalUsage.settings';
import ClusterHistoricalUsageChart from 'pages/Cluster/ClusterHistoricalUsageChart';
import { getUserResources } from 'services/api';
import { UserResources, UserUsage } from 'types';
import handleError from 'utils/error';
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
  UsageRow,
} from './usage';
import UsageCalendar from './UsageCalendar';
import usePageHidden from './usePageHidden';
import css from './UserOverview.module.scss';
import useUserUsage from './useUserUsage';

export const RESOURCES_POLL_INTERVAL = 15_000;

const MODE_OPTIONS = [
  { id: ResourceMode.Gpu, label: 'GPU' },
  { id: ResourceMode.Aux, label: 'Aux' },
  { id: ResourceMode.Both, label: 'GPU+Aux' },
];

interface ResourcesProps {
  projection: UsageProjection;
  resources: Loadable<UserResources>;
}

const CurrentResources: React.FC<ResourcesProps> = ({ projection, resources }) => (
  <Section loading={Loadable.isNotLoaded(resources)} title="Current resources">
    {Loadable.match(resources, {
      Failed: () => null,
      Loaded: (resources) => (
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
  /** The start of the UTC day the views end with. */
  now: number;
  projection: UsageProjection;
  usage: UserUsage;
}

const UsageView: React.FC<UsageProps> = ({ groupBy, now, projection, usage }) => {
  const view = useMemo(
    () => projection.usage(groupBy, usage.entries, now),
    [groupBy, now, projection, usage.entries],
  );
  const dateRange = useMemo((): [number, number] => {
    // The chart reads its time strings as local dates, and so does its range.
    const today = todayUtc(now);
    const [first, format] =
      groupBy === GroupBy.Month
        ? [today.subtract(USAGE_MONTHS - 1, 'month'), MONTH_FORMAT]
        : [today.subtract(USAGE_DAYS - 1, 'day'), DAY_FORMAT];
    return [dayjs(first.format(format)).unix(), dayjs(today.format(format)).unix()];
  }, [groupBy, now]);

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

interface HistoryProps {
  /** The start of the UTC day the views end with. */
  now: number;
  projection: UsageProjection;
  usage: Loadable<UserUsage>;
}

/* The history blocks take stable props and skip the renders of the resources polls. */

/** The calendar of the active days. */
const ActiveDays: React.FC<HistoryProps> = React.memo(({ now, projection, usage }) => {
  const calendar = useMemo(
    () => Loadable.map(usage, (usage) => projection.calendar(usage.entries, now)),
    [now, projection, usage],
  );

  return (
    <Section
      id="active-days"
      loading={Loadable.isNotLoaded(calendar)}
      title={Loadable.match(calendar, {
        _: () => 'Active days',
        Loaded: ({ activeDays }) => `Active days · ${activeDays}`,
      })}>
      {Loadable.match(calendar, {
        _: () => null,
        Loaded: ({ weeks }) => <UsageCalendar dayValues={projection.day} weeks={weeks} />,
      })}
    </Section>
  );
});

/** The usage by day or by month. */
const Usage: React.FC<HistoryProps> = React.memo(({ now, projection, usage }) => {
  const [groupBy, setGroupBy] = useState<GroupBy>(GroupBy.Day);

  return (
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
        _: () => null,
        Loaded: (usage) => (
          <UsageView groupBy={groupBy} now={now} projection={projection} usage={usage} />
        ),
      })}
    </Section>
  );
});

/** Whether any day of the usage holds Aux time. */
const hasAuxUsage = (usage: UserUsage): boolean =>
  usage.entries.some((entry) => entry.auxContainerSeconds > 0 || entry.auxActiveSeconds > 0);

/** The overview of one user; a new user starts over. */
const Overview: React.FC<{ userId: number }> = ({ userId }) => {
  const [resources, setResources] = useState<Loadable<UserResources>>(NotLoaded);
  const [mode, setMode] = useState<ResourceMode>(ResourceMode.Both);
  const { now, usage } = useUserUsage(userId);
  const hidden = usePageHidden();
  const hasLoaded = useRef(false);
  const hasFailed = useRef(false);

  const fetchResources = useCallback(
    async (signal: AbortSignal) => {
      try {
        const response = await getUserResources({ userId }, { signal });
        if (signal.aborted) return;
        hasLoaded.current = true;
        setResources(Loaded(response));
      } catch (e) {
        if (signal.aborted || isAborted(e)) return;
        // A failed refresh keeps the last counts; a toast for the first failure only.
        handleError(e, {
          publicSubject: 'Unable to fetch current resources.',
          silent: hasLoaded.current || hasFailed.current,
        });
        hasFailed.current = true;
        if (!hasLoaded.current) setResources(Failed(e as Error));
      }
    },
    [userId],
  );

  // Every 15 s while the page is visible, from the end of the previous request; hiding the page
  // or leaving it cancels the request in flight.
  useEffect(() => {
    if (hidden) return;
    const canceler = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      await fetchResources(canceler.signal);
      if (!canceler.signal.aborted) timer = setTimeout(poll, RESOURCES_POLL_INTERVAL);
    };
    poll();
    return () => {
      canceler.abort();
      clearTimeout(timer);
    };
  }, [fetchResources, hidden]);

  // Aux is left out only where the cluster is known not to meter it and the usage has none.
  const usageHasAux = useMemo(
    () => Loadable.match(usage, { _: () => undefined, Loaded: hasAuxUsage }),
    [usage],
  );
  const auxAbsent =
    Loadable.match(resources, { _: () => false, Loaded: (resources) => !resources.auxMetered }) &&
    usageHasAux === false;
  const shownMode = auxAbsent ? ResourceMode.Gpu : mode;
  const projection = useMemo(() => usageProjection(shownMode), [shownMode]);

  return (
    <div className={css.base} data-testid="user-overview">
      {!auxAbsent && (
        <div className={css.mode}>
          <RadioGroup<ResourceMode> options={MODE_OPTIONS} value={mode} onChange={setMode} />
        </div>
      )}
      <CurrentResources projection={projection} resources={resources} />
      <ActiveDays now={now} projection={projection} usage={usage} />
      <Usage now={now} projection={projection} usage={usage} />
    </div>
  );
};

interface Props {
  userId: number;
}

/**
 * A user's current resources, active days and usage, in one resource mode: GPU, Aux or both,
 * where the cluster meters Aux containers or the usage holds Aux time.
 */
const UserOverview: React.FC<Props> = ({ userId }: Props) => (
  <Overview key={userId} userId={userId} />
);

export default UserOverview;
