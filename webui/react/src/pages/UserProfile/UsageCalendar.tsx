import Tooltip from 'hew/Tooltip';
import React, { useMemo, useState } from 'react';

import { UserUsage } from 'types';

import { CalendarDay, calendarWeeks, formatHours } from './usage';
import css from './UsageCalendar.module.scss';

interface Props {
  showCpuSlots: boolean;
  usage: UserUsage;
}

const DayLine: React.FC<{ day: CalendarDay; showCpuSlots: boolean }> = ({ day, showCpuSlots }) => (
  <div className={css.day} data-testid="usage-calendar-day">
    <span>{day.date}</span>
    {day.entry && (
      <>
        <span>GPU hours {formatHours(day.entry.gpuSeconds)}</span>
        {showCpuSlots && <span>CPU slot hours {formatHours(day.entry.cpuSlotSeconds)}</span>}
        <span>
          Aux container hours{' '}
          {day.entry.auxContainerSeconds === undefined
            ? 'Not available'
            : formatHours(day.entry.auxContainerSeconds)}
        </span>
      </>
    )}
  </div>
);

/** The UTC days of the last 53 weeks, a column a week from Monday, by the time anything was held. */
const UsageCalendar: React.FC<Props> = ({ showCpuSlots, usage }: Props) => {
  const weeks = useMemo(() => calendarWeeks(usage.entries), [usage.entries]);
  const [selectedDate, setSelectedDate] = useState<string>();
  const selected = useMemo(
    () =>
      selectedDate === undefined
        ? undefined
        : weeks.flatMap((week) => week.days).find((day) => day.date === selectedDate),
    [selectedDate, weeks],
  );

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
              <Tooltip content={day.date} key={day.date}>
                <div
                  aria-label={day.date}
                  className={classes.join(' ')}
                  data-date={day.date}
                  role="button"
                  tabIndex={0}
                  onClick={() => setSelectedDate(day.date)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') setSelectedDate(day.date);
                  }}
                />
              </Tooltip>
            );
          }),
        )}
      </div>
      {selected && <DayLine day={selected} showCpuSlots={showCpuSlots} />}
    </div>
  );
};

export default UsageCalendar;
