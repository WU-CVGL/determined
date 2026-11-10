import Tooltip from 'hew/Tooltip';
import React, { useLayoutEffect, useMemo, useRef, useState } from 'react';

import { CalendarDay, CalendarWeek, UsageValue } from './usage';
import css from './UsageCalendar.module.scss';

interface Props {
  /** The values of a day, shown under the grid when it is selected. */
  dayValues: (day: CalendarDay) => UsageValue[];
  weeks: CalendarWeek[];
}

const DayLine: React.FC<{ day: CalendarDay; values: UsageValue[] }> = ({ day, values }) => (
  <div className={css.day} data-testid="usage-calendar-day">
    <span>{day.date} (UTC)</span>
    {values.map(({ title, value }) => (
      <span key={title}>
        {title} {value}
      </span>
    ))}
  </div>
);

/** The UTC days of the last 53 weeks, a column a week from Monday, by level. */
const UsageCalendar: React.FC<Props> = ({ dayValues, weeks }: Props) => {
  const [selectedDate, setSelectedDate] = useState<string>();
  const selected = useMemo(
    () =>
      selectedDate === undefined
        ? undefined
        : weeks.flatMap((week) => week.days).find((day) => day.date === selectedDate),
    [selectedDate, weeks],
  );

  // The newest weeks are at the right end: a grid wider than the page starts there.
  const scroll = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    if (scroll.current) scroll.current.scrollLeft = scroll.current.scrollWidth;
  }, [weeks.length]);

  return (
    <div className={css.base} data-testid="usage-calendar">
      <div className={css.scroll} ref={scroll}>
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
      </div>
      {selected && <DayLine day={selected} values={dayValues(selected)} />}
    </div>
  );
};

export default UsageCalendar;
