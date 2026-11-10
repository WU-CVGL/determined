import dayjs from 'dayjs';
import Card from 'hew/Card';
import RadioGroup from 'hew/RadioGroup';
import { Failed, Loadable, Loaded, NotLoaded } from 'hew/utils/loadable';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import OverviewStats from 'components/OverviewStats';
import Section from 'components/Section';
import ResponsiveTable from 'components/Table/ResponsiveTable';
import usePolling from 'hooks/usePolling';
import { GroupBy } from 'pages/Cluster/ClusterHistoricalUsage.settings';
import ClusterHistoricalUsageChart from 'pages/Cluster/ClusterHistoricalUsageChart';
import { getUserResources } from 'services/api';
import { UserResources, UserUsage } from 'types';
import handleError from 'utils/error';

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
import css from './UserOverview.module.scss';
import useUserUsage from './useUserUsage';

export const RESOURCES_POLL_INTERVAL = 15_000;

const MODE_OPTIONS = [
  { id: ResourceMode.Gpu, label: 'GPU' },
  { id: ResourceMode.Aux, label: 'Aux' },
  { id: ResourceMode.Both, label: 'GPU+Aux' },
];

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

interface UsageSectionsProps {
  projection?: UsageProjection;
  userId: number;
}

/** The calendar and the usage; empty until the projection is known. */
const UsageSections: React.FC<UsageSectionsProps> = ({ projection, userId }) => {
  const { now, usage } = useUserUsage(userId);
  const [groupBy, setGroupBy] = useState<GroupBy>(GroupBy.Day);

  const shown = useMemo(
    () =>
      projection
        ? Loadable.map(usage, (usage) => ({ projection, usage }))
        : Loadable.flatMap(usage, () => NotLoaded),
    [projection, usage],
  );
  const calendar = useMemo(
    () => Loadable.map(shown, ({ projection, usage }) => projection.calendar(usage.entries, now)),
    [now, shown],
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
            <UsageView groupBy={groupBy} now={now} projection={projection} usage={usage} />
          ),
        })}
      </Section>
    </>
  );
};

/** The overview of one user; a new user starts over. */
const Overview: React.FC<{ userId: number }> = ({ userId }) => {
  const [resources, setResources] = useState<Loadable<UserResources>>(NotLoaded);
  const [mode, setMode] = useState<ResourceMode>(ResourceMode.Both);
  const hasLoaded = useRef(false);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const fetchResources = useCallback(async () => {
    try {
      const response = await getUserResources({ userId });
      if (!mounted.current) return;
      hasLoaded.current = true;
      setResources(Loaded(response));
    } catch (e) {
      if (!mounted.current) return;
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
    () => (auxMetered === undefined ? undefined : usageProjection(mode, auxMetered)),
    [auxMetered, mode],
  );

  return (
    <div className={css.base} data-testid="user-overview">
      {auxMetered && (
        <div className={css.mode}>
          <RadioGroup<ResourceMode> options={MODE_OPTIONS} value={mode} onChange={setMode} />
        </div>
      )}
      <CurrentResources projection={projection} resources={resources} />
      <UsageSections projection={projection} userId={userId} />
    </div>
  );
};

interface Props {
  userId: number;
}

/**
 * A user's current resources, active days and usage, in one resource mode: GPU, Aux or both,
 * where the cluster meters Aux containers.
 */
const UserOverview: React.FC<Props> = ({ userId }: Props) => (
  <Overview key={userId} userId={userId} />
);

export default UserOverview;
