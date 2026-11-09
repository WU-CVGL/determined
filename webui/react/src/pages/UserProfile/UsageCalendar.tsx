import Tooltip from 'hew/Tooltip';
import React, { useMemo } from 'react';

import { UserUsage } from 'types';

import { CalendarDay, calendarWeeks, formatHours } from './usage';
import css from './UsageCalendar.module.scss';

interface Props {
  showCpuSlots: boolean;
  usage: UserUsage;
}

const DayTooltip: React.FC<{ day: CalendarDay; showCpuSlots: boolean }> = ({
  day,
  showCpuSlots,
}) => (
  <div>
    <div>{day.date}</div>
    {day.entry && (
      <>
        <div>GPU hours {formatHours(day.entry.gpuSeconds)}</div>
        {showCpuSlots && <div>CPU slot hours {formatHours(day.entry.cpuSlotSeconds)}</div>}
        <div>
          Aux container hours{' '}
          {day.entry.auxContainerSeconds === undefined
            ? 'Not available'
            : formatHours(day.entry.auxContainerSeconds)}
        </div>
      </>
    )}
  </div>
);

/** The UTC days of the last 53 weeks, a column a week from Monday, by the time anything was held. */
const UsageCalendar: React.FC<Props> = ({ showCpuSlots, usage }: Props) => {
  const weeks = useMemo(() => calendarWeeks(usage.entries), [usage.entries]);

  return (
    <div className={css.base} data-testid="usage-calendar">
      <div className={css.months}>
        {weeks.map((week, index) => (
          <span key={index} style={{ gridColumn: index + 1 }}>
            {week.monthLabel}
          </span>
        ))}
      </div>
      <div className={css.grid}>
        {weeks.flatMap((week) =>
          week.days.map((day) => {
            const classes = [css.cell];
            classes.push(day.entry ? css[`level${day.level}`] : css.notCovered);
            return (
              <Tooltip
                content={<DayTooltip day={day} showCpuSlots={showCpuSlots} />}
                key={day.date}>
                <div
                  aria-label={day.date}
                  className={classes.join(' ')}
                  data-date={day.date}
                  role="img"
                />
              </Tooltip>
            );
          }),
        )}
      </div>
    </div>
  );
};

export default UsageCalendar;
